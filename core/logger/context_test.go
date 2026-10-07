package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

type requestKey struct{}

// captureDefault points slog.Default at a JSON buffer for the test and restores it after.
func captureDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func setExtractor(t *testing.T, fn ContextAttrs) {
	t.Helper()
	SetContextAttrs(fn)
	t.Cleanup(func() { SetContextAttrs(nil) })
}

func lastLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	var m map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &m); err != nil {
		t.Fatalf("decode log line %q: %v", lines[len(lines)-1], err)
	}
	return m
}

func TestFromContext_AddsTheExtractedAttributes(t *testing.T) {
	buf := captureDefault(t)
	setExtractor(t, func(ctx context.Context) []slog.Attr {
		id, _ := ctx.Value(requestKey{}).(string)
		if id == "" {
			return nil
		}
		return []slog.Attr{slog.String("request_id", id), slog.String("user_id", "u-1")}
	})

	ctx := context.WithValue(context.Background(), requestKey{}, "req-7")
	FromContext(ctx).Warn("upstream failed", "err", "boom")

	line := lastLine(t, buf)
	for k, want := range map[string]string{"msg": "upstream failed", "err": "boom", "request_id": "req-7", "user_id": "u-1"} {
		if line[k] != want {
			t.Errorf("%s = %v, want %q (line %v)", k, line[k], want, line)
		}
	}
}

func TestFromContext_IsTheDefaultLoggerWhenThereIsNothingToAdd(t *testing.T) {
	_ = captureDefault(t)

	SetContextAttrs(nil)
	if FromContext(context.Background()) != slog.Default() {
		t.Error("without an extractor FromContext must return slog.Default()")
	}

	calls := 0
	setExtractor(t, func(context.Context) []slog.Attr { calls++; return nil })
	if FromContext(context.Background()) != slog.Default() {
		t.Error("a context the extractor knows nothing about must give slog.Default()")
	}
	if calls != 1 {
		t.Errorf("extractor called %d times, want 1", calls)
	}
	var nilCtx context.Context
	if FromContext(nilCtx) != slog.Default() {
		t.Error("a nil context must give slog.Default()")
	}
	if calls != 1 {
		t.Errorf("the extractor must not see a nil context (called %d times)", calls)
	}
}

// TestFromContext_FollowsTheCurrentDefault: the logger is derived per call, so a handler installed after startup
// (logger.InitWith, a test capturing output) receives the lines.
func TestFromContext_FollowsTheCurrentDefault(t *testing.T) {
	setExtractor(t, func(context.Context) []slog.Attr { return []slog.Attr{slog.String("request_id", "r")} })
	ctx := context.Background()
	first := captureDefault(t)
	FromContext(ctx).Error("one")
	second := captureDefault(t)
	FromContext(ctx).Error("two")

	if lastLine(t, first)["msg"] != "one" || lastLine(t, second)["msg"] != "two" {
		t.Errorf("lines went to the wrong handler: first %q, second %q", first.String(), second.String())
	}
	if lastLine(t, second)["request_id"] != "r" {
		t.Errorf("request_id missing after the default changed: %s", second.String())
	}
}
