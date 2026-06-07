package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/models"
)

// countingNotifier records how many events it sees, so a test can assert that
// a cancelled-before-begin stream emits nothing.
type countingNotifier struct{count int64}

func (n *countingNotifier) Emit(name string, data any) {atomic.AddInt64(&n.count, 1)}

// TestStreamChatMessage_CancelBeforeBegin_ReleasesGoroutine verifies that a
// stream which the client never starts (no /api/chat/begin) is released when
// it is explicitly cancelled, instead of blocking on `<-ctl.started` until the
// 5-minute upper-bound timeout.
func TestStreamChatMessage_CancelBeforeBegin_ReleasesGoroutine(t *testing.T) {
	notifier := &countingNotifier{}

	factory := ai.NewProviderFactory(
		func(scope, provider string) (string, error) {
			return "", fmt.Errorf("no key configured")
		},
		nil,
		nil,
	)

	svc := &ChatService{
		notifier: notifier,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:  factory,
	}

	id, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	svc.CancelStream(id)

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s after CancelStream")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := atomic.LoadInt64(&notifier.count); got != 0 {
		t.Errorf("expected 0 events emitted for cancelled-before-begin stream, got %d", got)
	}
}

// TestStreamChatMessage_CancelAfterBegin_EmitsError verifies the normal
// cancellation path.
func TestStreamChatMessage_CancelAfterBegin_EmitsError(t *testing.T) {
	notifier := &countingNotifier{}

	factory := ai.NewProviderFactory(
		func(scope, provider string) (string, error) {
			return "", fmt.Errorf("no key configured")
		},
		nil,
		nil,
	)

	svc := &ChatService{
		notifier: notifier,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:  factory,
	}

	id, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	svc.BeginStream(id)

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s after BeginStream")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := atomic.LoadInt64(&notifier.count); got == 0 {
		t.Errorf("expected at least one error event after BeginStream, got 0")
	}
}

func TestBeginStream_ConcurrentCalls_NoPanic(t *testing.T) {
	notifier := &countingNotifier{}
	factory := ai.NewProviderFactory(
		func(scope, provider string) (string, error) {return "", fmt.Errorf("no key")},
		nil,
		nil,
	)
	svc := &ChatService{
		notifier: notifier,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:  factory,
	}

	id, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	const N = 50
	done := make(chan struct{}, N)
	for range N {
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("BeginStream panicked: %v", rec)
				}
				done <- struct{}{}
			}()
			svc.BeginStream(id)
		}()
	}
	for range N {
		<-done
	}

	svc.CancelStream(id)
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestNormalizeChatParams(t *testing.T) {
	cases := []struct {
		name      string
		temp      float64
		maxTok    int
		ctxLimit  int
		wantTemp  float64
		wantMaxTok int
	}{
		{"in range untouched", 0.7, 1000, 128000, 0.7, 1000},
		{"temp below zero clamps to 0", -1, 100, 0, 0, 100},
		{"temp above two clamps to 2", 5, 100, 0, 2, 100},
		{"negative maxtokens clamps to 0", 0.5, -10, 0, 0.5, 0},
		{"maxtokens over context window is capped", 0.5, 999999, 8000, 0.5, 8000 - contextReserve},
		{"unknown ctxlimit leaves maxtokens", 0.5, 999999, 0, 0.5, 999999},
		{"tiny context window floors cap at 0", 0.5, 100, 1000, 0.5, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotTemp, gotMaxTok := normalizeChatParams(c.temp, c.maxTok, c.ctxLimit)
			if gotTemp != c.wantTemp {
				t.Errorf("temp = %v, want %v", gotTemp, c.wantTemp)
			}
			if gotMaxTok != c.wantMaxTok {
				t.Errorf("maxTokens = %v, want %v", gotMaxTok, c.wantMaxTok)
			}
		})
	}
}
