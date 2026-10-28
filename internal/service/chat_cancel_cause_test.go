package service

import (
	"bufio"
	"context"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/testutil"
)

// R-observability-4: a stream's cancellation carries its stopper's reason as the context's cause, so the AI layer
// counts the cancelled provider call under pad_ai_requests_cancelled_total{reason} and never as a provider error.

// blockingStreamProvider streams nothing until its context ends, then fails the way the SSE readers do, and records
// the cancel cause it saw.
type blockingStreamProvider struct {
	*testutil.FakeProvider
	once    sync.Once
	started chan struct{}
	mu      sync.Mutex
	cause   error
}

func newBlockingStreamProvider() *blockingStreamProvider {
	return &blockingStreamProvider{FakeProvider: &testutil.FakeProvider{Fallback: &testutil.FakeTurn{Text: "never"}}, started: make(chan struct{})}
}

func (p *blockingStreamProvider) Stream(ctx context.Context, _ ai.Request, _ func(ai.Chunk)) error {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	p.mu.Lock()
	p.cause = context.Cause(ctx)
	p.mu.Unlock()
	return fmt.Errorf("reading openai SSE stream: %w", ctx.Err())
}

func (p *blockingStreamProvider) seenReason(t *testing.T) ai.CancelReason {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	reason, ok := ai.CancelReasonOf(p.cause)
	if !ok {
		t.Fatalf("the provider's context was cancelled with %v, not a reason", p.cause)
	}
	return reason
}

// aiSeries sums the exposition lines of family whose labels include every one of want.
func aiSeries(t *testing.T, family string, want ...string) float64 {
	t.Helper()
	rr := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	var sum float64
	sc := bufio.NewScanner(rr.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
lines:
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, family+"{") {
			continue
		}
		for _, w := range want {
			if !strings.Contains(line, w) {
				continue lines
			}
		}
		v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		sum += v
	}
	return sum
}

func TestStreamCancel_TheStoppersReasonReachesTheProviderCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(svc *ChatService, id string)
		want ai.CancelReason
	}{
		{"user stop", func(svc *ChatService, id string) { svc.CancelStream(id) }, ai.CancelClient},
		{"shutdown", func(svc *ChatService, _ string) { svc.CancelAll() }, ai.CancelShutdown},
		{"idle watchdog", nil, ai.CancelIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := "cancel-cause-" + string(tc.want)
			p := newBlockingStreamProvider()
			svc := recordService(t, provider, p)
			if tc.stop == nil {
				svc.watchdogInterval, svc.idleTimeout, svc.hostedIdleTimeout = 5*time.Millisecond, 40*time.Millisecond, 40*time.Millisecond
			}
			label := `provider="` + provider + `"`
			errsBefore := aiSeries(t, "ai_request_errors_total", label)
			cancelledBefore := aiSeries(t, "pad_ai_requests_cancelled_total", label, `reason="`+string(tc.want)+`"`)

			id := startRecordedTurn(t, svc, provider)
			select {
			case <-p.started:
			case <-time.After(5 * time.Second):
				t.Fatal("the provider was never called")
			}
			if tc.stop != nil {
				tc.stop(svc, id)
			}
			waitStreamFinished(t, svc, id)

			if got := p.seenReason(t); got != tc.want {
				t.Errorf("the provider call was cancelled for %q, want %q", got, tc.want)
			}
			if d := aiSeries(t, "ai_request_errors_total", label) - errsBefore; d != 0 {
				t.Errorf("ai_request_errors_total rose by %v for a cancelled stream", d)
			}
			if d := aiSeries(t, "pad_ai_requests_cancelled_total", label, `reason="`+string(tc.want)+`"`) - cancelledBefore; d != 1 {
				t.Errorf("pad_ai_requests_cancelled_total{reason=%q} rose by %v, want 1", tc.want, d)
			}
		})
	}
}

func TestStreamCancelReason_EveryStopperCode(t *testing.T) {
	for code, want := range map[string]ai.CancelReason{
		"":                      ai.CancelClient,
		ChatErrStopped:          ai.CancelClient,
		ChatErrIdleTimeout:      ai.CancelIdle,
		ChatErrClientGone:       ai.CancelSubscriberGone,
		ChatErrServerRestarting: ai.CancelShutdown,
		ChatErrInterrupted:      ai.CancelShutdown,
	} {
		if got := streamCancelReason(code); got != want {
			t.Errorf("streamCancelReason(%q) = %q, want %q", code, got, want)
		}
	}
}
