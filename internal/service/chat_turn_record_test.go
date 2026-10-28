package service

// R-observability-1: every chat turn leaves one terminal record — a "chat turn ended" log line and a
// pad_chat_turns_total count — with the outcome the client saw; idle, disconnect and user stops are told apart.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"pad-analyzer/internal/ai"
	fake "pad-analyzer/internal/testutil"
	"pad-core/models"
)

// lockedBuffer is a log sink the workers write from their own goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) lines() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(b.buf.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// captureJSONLogs routes the default logger to a JSON sink for the test.
func captureJSONLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	sink := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return sink
}

// turnEndedLines are the stream's "chat turn ended" lines.
func turnEndedLines(sink *lockedBuffer, streamID string) []map[string]any {
	var out []map[string]any
	for _, l := range sink.lines() {
		if l["msg"] == "chat turn ended" && l["stream_id"] == streamID {
			out = append(out, l)
		}
	}
	return out
}

func histogramCount(t *testing.T, h prometheus.Observer) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := h.(prometheus.Metric).Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

func turnsCounted(outcome string) float64 {
	return testutil.ToFloat64(chatTurnsTotal.WithLabelValues(outcome))
}

func recordService(t *testing.T, provider string, p ai.Provider) *ChatService {
	t.Helper()
	doc := &models.FlowDocument{ID: "f-record", Name: "f"}
	return failedTurnService(t, provider, p, doc)
}

func startRecordedTurn(t *testing.T, svc *ChatService, provider string) string {
	t.Helper()
	req := models.ChatRequest{Provider: provider, UserMessage: "what does it do?", ConversationID: uuid.NewString(),
		UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString()}
	id, err := svc.StreamChatMessage(context.Background(), "test", &models.FlowDocument{ID: "f-record", Name: "f"}, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.BeginStream(context.Background(), id)
	return id
}

func TestTurnRecord_ADoneTurnIsLoggedAndCountedOnce(t *testing.T) {
	sink := captureJSONLogs(t)
	p := &fake.FakeProvider{Fallback: &fake.FakeTurn{Text: "the answer", TokensIn: 3, TokensOut: 5}}
	svc := recordService(t, "record-done", p)
	before, beforeDur := turnsCounted("done"), histogramCount(t, chatTurnDuration)

	id := startRecordedTurn(t, svc, "record-done")
	waitStreamFinished(t, svc, id)

	lines := turnEndedLines(sink, id)
	if len(lines) != 1 {
		t.Fatalf("%d chat turn ended lines, want 1: %v", len(lines), lines)
	}
	l := lines[0]
	if l["outcome"] != "done" || l["provider"] != "record-done" || l["tokens_in"] != float64(3) || l["tokens_out"] != float64(5) {
		t.Errorf("line %v, want outcome done, the provider and the turn's tokens", l)
	}
	for _, key := range []string{"duration_ms", "flow_id", "conversation", "tool_calls", "iterations"} {
		if _, ok := l[key]; !ok {
			t.Errorf("line %v has no %s", l, key)
		}
	}
	if got := turnsCounted("done"); got != before+1 {
		t.Errorf("done turns counted %v, want %v", got, before+1)
	}
	if got := histogramCount(t, chatTurnDuration); got != beforeDur+1 {
		t.Errorf("turn durations observed %d, want %d", got, beforeDur+1)
	}
}

func TestTurnRecord_AFailedTurnCarriesItsCode(t *testing.T) {
	sink := captureJSONLogs(t)
	p := &fake.FakeProvider{Fallback: &fake.FakeTurn{Err: fmt.Errorf("upstream said 429: %w", ai.ErrRateLimited)}}
	svc := recordService(t, "record-ratelimited", p)
	before := turnsCounted("provider_rate_limited")

	id := startRecordedTurn(t, svc, "record-ratelimited")
	waitStreamFinished(t, svc, id)

	lines := turnEndedLines(sink, id)
	if len(lines) != 1 || lines[0]["outcome"] != "provider_rate_limited" || lines[0]["code"] != ChatErrProviderRateLimited {
		t.Fatalf("lines %v, want one with outcome provider_rate_limited and its code", lines)
	}
	if got := turnsCounted("provider_rate_limited"); got != before+1 {
		t.Errorf("counted %v, want %v", got, before+1)
	}
}

// The watchdog's idle and disconnect cancellations and a user's stop each have their own wire code
// (R-streaming-10), and keep the outcome labels an operator reads.
func TestTurnRecord_IdleDisconnectAndStopAreDistinctOutcomes(t *testing.T) {
	for _, tc := range []struct {
		outcome, code string
		setup         func(svc *ChatService)
		act           func(svc *ChatService, id string)
	}{
		{"idle_timeout", "STREAM_IDLE_TIMEOUT", func(svc *ChatService) {
			svc.watchdogInterval, svc.idleTimeout = 5*time.Millisecond, 40*time.Millisecond
		}, nil},
		{"subscriber_gone", "STREAM_CLIENT_GONE", func(svc *ChatService) {
			svc.watchdogInterval, svc.idleTimeout = 5*time.Millisecond, time.Hour
			svc.notifier.(*fake.CountingNotifier).SetNoSubscriber(true)
		}, nil},
		{"stopped", ChatErrStopped, func(svc *ChatService) {
			svc.watchdogInterval, svc.idleTimeout = 5*time.Millisecond, time.Hour
		}, func(svc *ChatService, id string) { svc.CancelStream(id) }},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			sink := captureJSONLogs(t)
			p := &fake.FakeProvider{Fallback: &fake.FakeTurn{Text: "partial", Block: true}, HonourCtx: true}
			provider := "record-" + tc.outcome
			svc := recordService(t, provider, p)
			tc.setup(svc)
			before := turnsCounted(tc.outcome)

			id := startRecordedTurn(t, svc, provider)
			if tc.act != nil {
				time.Sleep(30 * time.Millisecond)
				tc.act(svc, id)
			}
			ctl := waitStreamFinished(t, svc, id)

			ctl.mu.Lock()
			code := ctl.errCode
			ctl.mu.Unlock()
			if code != tc.code {
				t.Errorf("the client saw %q, want %s", code, tc.code)
			}
			lines := turnEndedLines(sink, id)
			if len(lines) != 1 || lines[0]["outcome"] != tc.outcome {
				t.Fatalf("lines %v, want one with outcome %s", lines, tc.outcome)
			}
			if got := turnsCounted(tc.outcome); got != before+1 {
				t.Errorf("counted %v, want %v", got, before+1)
			}
		})
	}
}

// A stream the stop ended in its stuck worker's place is recorded then, once: the late worker adds no second line.
func TestTurnRecord_AShutdownForcedEndIsRecordedOnce(t *testing.T) {
	sink := captureJSONLogs(t)
	p := &lateProvider{FakeProvider: &fake.FakeProvider{}, done: true, release: make(chan struct{})}
	t.Cleanup(p.unpark)
	svc, n := shutdownTestService(t, "record-forced", p)
	svc.shutdownBudget = 50 * time.Millisecond
	id := startLiveStream(t, svc, n, "record-forced")
	before := turnsCounted("server_restarting")

	if err := svc.Shutdown(context.Background()); err == nil {
		t.Fatal("Shutdown reported no error with a worker still running")
	}
	if lines := turnEndedLines(sink, id); len(lines) != 1 || lines[0]["outcome"] != "server_restarting" {
		t.Fatalf("lines when Shutdown returned %v, want one with outcome server_restarting", lines)
	}
	p.unpark()
	waitStreamFinished(t, svc, id)
	if lines := turnEndedLines(sink, id); len(lines) != 1 {
		t.Errorf("%d lines after the late worker ended, want still 1", len(lines))
	}
	if got := turnsCounted("server_restarting"); got != before+1 {
		t.Errorf("counted %v, want %v", got, before+1)
	}
}

// The tool and approval histograms are fed by a tool turn: one tool duration per executed tool, one approval wait
// per decided card.
func TestTurnRecord_ToolAndApprovalDurations(t *testing.T) {
	doc := &models.FlowDocument{ID: "f-record", Name: "f"}
	p := &fake.FakeProvider{Tools: true, Turns: []fake.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "c1", Name: "list_findings", Input: json.RawMessage(`{}`)}}},
		{Text: "done"},
	}}
	svc := failedTurnService(t, "record-tools", p, doc)
	toolBefore := histogramCount(t, chatToolDuration.WithLabelValues("list_findings"))
	_, ctl := streamTurn(t, svc, doc, "record-tools", true)
	if ctl == nil {
		t.Fatal("no stream")
	}
	if got := histogramCount(t, chatToolDuration.WithLabelValues("list_findings")); got != toolBefore+1 {
		t.Errorf("list_findings durations observed %d, want %d", got, toolBefore+1)
	}
}

func TestTurnRecord_ApprovalWaitIsObserved(t *testing.T) {
	sink := captureJSONLogs(t)
	doc := fixTestDoc()
	p := &fake.FakeProvider{Tools: true, Turns: []fake.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}}, TokensIn: 10, TokensOut: 5},
		{Text: "Fixed."},
	}}
	svc := failedTurnService(t, "record-approval", p, doc)
	before := histogramCount(t, chatApprovalWait.WithLabelValues("approved"))
	_, ctl := streamTurn(t, svc, doc, "record-approval", true)
	if got := histogramCount(t, chatApprovalWait.WithLabelValues("approved")); got != before+1 {
		t.Errorf("approved waits observed %d, want %d", got, before+1)
	}
	lines := turnEndedLines(sink, ctl.streamID)
	if len(lines) != 1 {
		t.Fatalf("lines %v, want 1", lines)
	}
	if lines[0]["tool_calls"] != float64(1) || lines[0]["approvals"] == nil {
		t.Errorf("line %v, want one tool call and the approvals by status", lines[0])
	}
}
