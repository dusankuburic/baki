package service

// R-observability-2: the chat's log lines go through logger.FromContext, so each carries the request's request_id,
// user_id and trace_id, and the stream's stream_id.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/reqinfo"
	fake "pad-analyzer/internal/testutil"
	"pad-core/logger"
	"pad-core/models"
)

// requestLogAttrs mirrors the server's extractor (middleware.LogAttrs), which this package cannot import.
func requestLogAttrs(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr
	if info := reqinfo.From(ctx); info != nil {
		attrs = append(attrs, slog.String("request_id", info.ID))
		if a := info.Actor(); a != nil {
			attrs = append(attrs, slog.String("user_id", a.UserID))
		}
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()))
	}
	return attrs
}

func TestChatLogs_CarryTheRequestTheUserTheTraceAndTheStream(t *testing.T) {
	for name, tc := range map[string]struct {
		turn      fake.FakeTurn
		failLoads int
	}{
		// A history that cannot be read (a Warn from the conversation layer), then the turn's record.
		"history failure": {fake.FakeTurn{Text: "unused"}, 1},
		// A failing provider, then the turn's record.
		"failing provider": {fake.FakeTurn{Err: errors.New("simulated: upstream 503")}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			p := &fake.FakeProvider{Fallback: &tc.turn}
			backend := &faultyConvBackend{FakeBackend: fake.NewFakeBackend(), failLoads: tc.failLoads}
			assertTurnLogsCarryContext(t, durabilityService(t, "log-context-"+strings.ReplaceAll(name, " ", "-"), p, backend),
				"log-context-"+strings.ReplaceAll(name, " ", "-"), 1+tc.failLoads)
		})
	}
}

func assertTurnLogsCarryContext(t *testing.T, svc *ChatService, provider string, minLines int) {
	t.Helper()
	sink := captureJSONLogs(t)
	logger.SetContextAttrs(requestLogAttrs)
	t.Cleanup(func() { logger.SetContextAttrs(nil) })

	info := &reqinfo.Info{ID: "req-" + uuid.NewString()}
	info.SetActor(&auth.Actor{UserID: "user-7"})
	ctx := reqinfo.With(context.Background(), info)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1, 2, 3}, SpanID: trace.SpanID{4}, TraceFlags: trace.FlagsSampled})
	ctx = trace.ContextWithSpanContext(ctx, sc)

	req := durableRequest(provider)
	id, err := svc.StreamChatMessage(ctx, "test", &models.FlowDocument{ID: "f-durable", Name: "f"}, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.BeginStream(context.Background(), id)
	waitStreamFinished(t, svc, id)

	var chat []map[string]any
	for _, l := range sink.lines() {
		if msg, _ := l["msg"].(string); strings.HasPrefix(msg, "chat") {
			chat = append(chat, l)
		}
	}
	if len(chat) < minLines {
		t.Fatalf("chat lines %v, want the history warning and the turn's record", chat)
	}
	for _, l := range chat {
		want := map[string]string{"request_id": info.ID, "user_id": "user-7", "stream_id": id, "trace_id": sc.TraceID().String()}
		for k, v := range want {
			if l[k] != v {
				t.Errorf("line %q: %s = %v, want %q", l["msg"], k, l[k], v)
			}
		}
	}
}
