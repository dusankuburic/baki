package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// --- R-observability-4: provider errors by class, cancellations apart ---

// cancelStub is a provider whose calls end the way the test says.
type cancelStub struct {
	stubProvider
	id     string
	stream func(ctx context.Context) error
	chat   func(ctx context.Context) error
	embed  func(ctx context.Context) error
}

func (s *cancelStub) ID() string { return s.id }

func (s *cancelStub) Stream(ctx context.Context, _ Request, _ func(Chunk)) error {
	return s.stream(ctx)
}

func (s *cancelStub) Chat(ctx context.Context, _ Request) (*Response, error) { return nil, s.chat(ctx) }

func (s *cancelStub) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	return nil, s.embed(ctx)
}

// sseCancelled is what the SSE readers return when the caller's context ends mid-stream: the scanner's error, wrapped.
func sseCancelled(ctx context.Context) error {
	<-ctx.Done()
	return fmt.Errorf("reading openai SSE stream: %w", ctx.Err())
}

// errorsFor sums ai_request_errors_total over every class of provider.
func errorsFor(provider string) float64 {
	var n float64
	for _, class := range aiErrorClasses {
		n += testutil.ToFloat64(aiRequestErrorsTotal.WithLabelValues(provider, class))
	}
	return n
}

func cancelledFor(provider string, reason CancelReason) float64 {
	return testutil.ToFloat64(aiRequestsCancelledTotal.WithLabelValues(provider, string(reason)))
}

// A stream the caller cancelled (the user's stop, a watchdog, shutdown) is not a provider error: it is counted
// under its reason, read from the context's cancel cause.
func TestAuditedStream_ACancelledStreamIsCountedUnderItsReasonNotAsAnError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(context.CancelCauseFunc)
		want   CancelReason
	}{
		{"user stop", func(c context.CancelCauseFunc) { c(CancelCause(CancelClient)) }, CancelClient},
		{"plain cancel", func(c context.CancelCauseFunc) { c(nil) }, CancelClient},
		{"idle watchdog", func(c context.CancelCauseFunc) { c(CancelCause(CancelIdle)) }, CancelIdle},
		{"subscriber gone", func(c context.CancelCauseFunc) { c(CancelCause(CancelSubscriberGone)) }, CancelSubscriberGone},
		{"shutdown", func(c context.CancelCauseFunc) { c(CancelCause(CancelShutdown)) }, CancelShutdown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := "cancel-test-" + string(tc.want)
			ap := newAuditedProvider(&cancelStub{id: provider, stream: sseCancelled}, nil, "u1", provider)
			errsBefore, cancelledBefore := errorsFor(provider), cancelledFor(provider, tc.want)
			ctx, cancel := context.WithCancelCause(context.Background())
			go func() { time.Sleep(5 * time.Millisecond); tc.cancel(cancel) }()
			if err := ap.Stream(ctx, Request{Model: "m"}, func(Chunk) {}); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the cancellation", err)
			}
			if d := errorsFor(provider) - errsBefore; d != 0 {
				t.Errorf("ai_request_errors_total rose by %v for a cancelled stream", d)
			}
			if d := cancelledFor(provider, tc.want) - cancelledBefore; d != 1 {
				t.Errorf("pad_ai_requests_cancelled_total{reason=%q} rose by %v, want 1", tc.want, d)
			}
		})
	}
}

// A deadline (the RAG query embed's 800 ms, the stream's 10-minute cap) is a cancellation too, reason deadline.
func TestAuditedEmbed_ADeadlineIsCountedAsACancellation(t *testing.T) {
	const provider = "cancel-test-embed"
	ap := newAuditedProvider(&cancelStub{id: provider, embed: func(ctx context.Context) error {
		<-ctx.Done()
		return fmt.Errorf("embeddings request: %w", ctx.Err())
	}}, nil, "u1", provider)
	errsBefore, cancelledBefore := errorsFor(provider), cancelledFor(provider, CancelDeadline)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := ap.Embed(ctx, []string{"q"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := errorsFor(provider) - errsBefore; d != 0 {
		t.Errorf("ai_request_errors_total rose by %v for a deadline", d)
	}
	if d := cancelledFor(provider, CancelDeadline) - cancelledBefore; d != 1 {
		t.Errorf("cancelled{reason=deadline} rose by %v, want 1", d)
	}
}

// A provider error that arrives while the context is done still counts as the provider's: a sentinel is the provider's
// answer, not the cancellation.
func TestAuditedChat_AProviderSentinelCountsEvenWhenTheContextIsDone(t *testing.T) {
	const provider = "cancel-test-sentinel"
	ap := newAuditedProvider(&cancelStub{id: provider, chat: func(context.Context) error { return &RateLimitError{} }}, nil, "u1", provider)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := testutil.ToFloat64(aiRequestErrorsTotal.WithLabelValues(provider, "rate_limited"))
	_, _ = ap.Chat(ctx, Request{Model: "m"})
	if d := testutil.ToFloat64(aiRequestErrorsTotal.WithLabelValues(provider, "rate_limited")) - before; d != 1 {
		t.Errorf("errors{class=rate_limited} rose by %v, want 1", d)
	}
}

// A 429 after the retries and a 401 are counted by class.
func TestAuditedChat_ProviderErrorsAreCountedByClass(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  string
	}{
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusUnauthorized, "auth"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"message":"no"}}`)
			}))
			t.Cleanup(srv.Close)
			provider := "class-test-" + tc.class
			inner := NewRetryingProviderWithConfig(compatErrProvider(srv.Client(), srv.URL+"/chat/completions"), 2, time.Millisecond)
			ap := newAuditedProvider(inner, nil, "u1", provider)
			before := testutil.ToFloat64(aiRequestErrorsTotal.WithLabelValues(provider, tc.class))
			if _, err := ap.Chat(context.Background(), compatChatReq()); err == nil {
				t.Fatal("want an error")
			}
			if d := testutil.ToFloat64(aiRequestErrorsTotal.WithLabelValues(provider, tc.class)) - before; d != 1 {
				t.Errorf("errors{class=%q} rose by %v, want 1", tc.class, d)
			}
			if tc.class == "rate_limited" && calls.Load() < 2 {
				t.Errorf("the 429 was tried %d times, want the retries first", calls.Load())
			}
		})
	}
}

func TestAIErrorClass_FromTheSentinels(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&RateLimitError{}, "rate_limited"},
		{ErrProviderDown, "provider_down"},
		{&upstreamTimeoutError{cause: context.DeadlineExceeded}, "provider_down"},
		{&circuitOpenError{}, "provider_down"},
		{ErrApiKeyInvalid, "auth"},
		{ErrKeyNotConfigured, "auth"},
		{&entraTokenRejectedError{msg: "x"}, "auth"},
		{&permissionDeniedError{msg: "no role"}, "permission"},
		{NewAccessDeniedError("region"), "permission"},
		{ErrInsufficientBalance, "permission"},
		{&ContextLimitError{Limit: 8}, "context_limit"},
		{ErrContentFiltered, "content_filtered"},
		{ErrOutputLimit, "output_limit"},
		{fmt.Errorf("dial: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), "transport"},
		{fmt.Errorf("openai stream truncated before terminal marker: %w", io.ErrUnexpectedEOF), "truncated"},
		{errors.New("something else"), "other"},
	} {
		if got := aiErrorClass(tc.err); got != tc.want {
			t.Errorf("aiErrorClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
		if !slices.Contains(aiErrorClasses, aiErrorClass(tc.err)) {
			t.Errorf("class %q is outside the closed set", aiErrorClass(tc.err))
		}
	}
}

// --- R-observability-14: repairs, fallbacks and Entra token events ---

func repairsCounted(provider, param string) float64 {
	return testutil.ToFloat64(aiParamRepairsTotal.WithLabelValues(provider, param))
}

// A request repaired once counts one repair; a later request with the memo pre-applied counts none.
func TestParamRepairs_CountedOncePerFreshRepair(t *testing.T) {
	resetLearnedRepairs()
	t.Cleanup(resetLearnedRepairs)
	srv := newRepairTestServer(t)
	srv.setReject(func(body map[string]any) (int, map[string]any) {
		if _, has := body["temperature"]; has {
			return rejectParam("temperature")
		}
		return 0, nil
	})
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")
	p.providerID = openaiSpec.ID
	req := Request{Model: "o4-mini", Temperature: 0.3, Messages: []Message{{Role: "user", Content: "hi"}}}
	before := repairsCounted("openai", "temperature")
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if d := repairsCounted("openai", "temperature") - before; d != 1 {
		t.Fatalf("a fresh repair counted %v, want 1", d)
	}
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if d := repairsCounted("openai", "temperature") - before; d != 1 {
		t.Errorf("a pre-applied memo counted again: %v in total, want 1", d)
	}
}

// A model that refuses streaming is answered non-streaming once per learning: kind stream_refused.
func TestRouteFallbacks_StreamRefusalIsCounted(t *testing.T) {
	resetLearnedRepairs()
	t.Cleanup(resetLearnedRepairs)
	srv := newRepairTestServer(t)
	srv.setReject(func(body map[string]any) (int, map[string]any) {
		if body["stream"] == true {
			return rejectParam("stream")
		}
		return 0, nil
	})
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")
	p.providerID = openaiSpec.ID
	before := testutil.ToFloat64(aiRouteFallbacksTotal.WithLabelValues("openai", fallbackStreamRefused))
	for range 2 {
		if err := p.Stream(context.Background(), Request{Model: "o1", Messages: []Message{{Role: "user", Content: "hi"}}}, func(Chunk) {}); err != nil {
			t.Fatal(err)
		}
	}
	if d := testutil.ToFloat64(aiRouteFallbacksTotal.WithLabelValues("openai", fallbackStreamRefused)) - before; d != 1 {
		t.Errorf("stream_refused rose by %v over a learning and a memoised turn, want 1", d)
	}
}

// refusingEntraCred refuses every token request.
type refusingEntraCred struct{}

func (refusingEntraCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errors.New("identity service down")
}

func entraEvents(kind string) float64 {
	return testutil.ToFloat64(entraTokenEventsTotal.WithLabelValues(kind))
}

func TestEntraTokenEvents_RefreshedFailedAndInvalidated(t *testing.T) {
	refreshed, failed, invalidated := entraEvents(entraTokenRefreshed), entraEvents(entraTokenRefreshFailed), entraEvents(entraTokenInvalidated)
	src := newEntraTokenSource(&fakeCred{})
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d := entraEvents(entraTokenRefreshed) - refreshed; d != 1 {
		t.Errorf("refreshed rose by %v, want 1", d)
	}
	src.Invalidate("not-the-cached-one")
	src.Invalidate(tok)
	if d := entraEvents(entraTokenInvalidated) - invalidated; d != 1 {
		t.Errorf("invalidated rose by %v, want 1 (a stale token is not an invalidation)", d)
	}
	if _, err := newEntraTokenSource(refusingEntraCred{}).Token(context.Background()); err == nil {
		t.Fatal("want the failure")
	}
	if d := entraEvents(entraTokenRefreshFailed) - failed; d != 1 {
		t.Errorf("refresh_failed rose by %v, want 1", d)
	}
}

// Every label value of the new families is in its closed set: the parameter set is repairableParams plus the
// tools-with-reasoning marker, and providers are registry ids (or "other").
func TestAIMetrics_LabelValuesAreClosedSets(t *testing.T) {
	for p := range repairableParams {
		if !slices.Contains(aiRepairParams(), p) {
			t.Errorf("repairable parameter %q is not a param label value", p)
		}
	}
	if !slices.Contains(aiRepairParams(), reasoningEffortNoneRepair) {
		t.Errorf("%q is not a param label value", reasoningEffortNoneRepair)
	}
	providers := []string{metricProviderOther}
	for _, s := range Specs() {
		providers = append(providers, s.ID)
	}
	for _, id := range providers[1:] {
		if got := metricProvider(id); got != id {
			t.Errorf("metricProvider(%q) = %q", id, got)
		}
	}
	if got := metricProvider("an unknown id"); got != metricProviderOther {
		t.Errorf("metricProvider(unknown) = %q, want %q", got, metricProviderOther)
	}
	// The OpenAI-compatible providers label their repairs with their own registry id.
	for _, b := range []struct {
		base *openaiBase
		id   string
	}{
		{&NewOpenAIProvider("k").openaiBase, openaiProviderID},
		{&NewXAIProvider("k").openaiBase, xaiSpec.ID},
		{&NewGLMProvider("k").openaiBase, glmSpec.ID},
		{&NewGitHubModelsProvider("k").openaiBase, githubModelsSpec.ID},
		{&NewCopilotProvider("k").openaiBase, copilotSpec.ID},
	} {
		if b.base.providerID != b.id {
			t.Errorf("%s provider labels its counters %q, want %q", b.base.providerLabel, b.base.providerID, b.id)
		}
	}
	// Exercise the recorders with values outside the sets: they must not escape.
	recordParamRepair("openai", "not-a-param")
	recordRouteFallback("not-a-provider", "not-a-kind")
	recordEntraTokenEvent("not-a-kind")
	recordAICancelled("cancel-test-closed", CancelReason("not-a-reason"))

	closed := map[string]map[string][]string{
		"pad_ai_param_repairs_total":      {"provider": providers, "param": append(aiRepairParams(), metricLabelOther)},
		"pad_ai_route_fallbacks_total":    {"provider": providers, "kind": aiRouteFallbackKinds},
		"pad_ai_entra_token_events_total": {"kind": entraTokenEventKinds},
		"pad_ai_requests_cancelled_total": {"reason": aiCancelReasons()},
		"ai_request_errors_total":         {"class": aiErrorClasses},
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(aiParamRepairsTotal, aiRouteFallbacksTotal, entraTokenEventsTotal, aiRequestsCancelledTotal, aiRequestErrorsTotal)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, mf := range families {
		sets, ok := closed[mf.GetName()]
		if !ok {
			continue
		}
		seen++
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if set, ok := sets[lp.GetName()]; ok && !slices.Contains(set, lp.GetValue()) {
					t.Errorf("%s{%s=%q} is outside its closed set", mf.GetName(), lp.GetName(), lp.GetValue())
				}
			}
		}
	}
	if seen != len(closed) {
		t.Errorf("gathered %d of the %d families", seen, len(closed))
	}
}

// Deterministic provider answers have classes of their own, and count as the provider's even when the caller's
// context ended meanwhile.
func TestAIErrorClass_DeterministicAnswersAreClassified(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{ErrToolsUnsupported, "invalid_request"},
		{ErrThoughtSignatureMissing, "invalid_request"},
		{fmt.Errorf("%w: bad document", ErrProviderConfigInvalid), "config"},
		{ErrEmbeddingDeploymentMissing, "config"},
		{ErrAzureAgentNotFound, "config"},
		{ErrProviderAccessDenied, "permission"},
	} {
		if got := aiErrorClass(tc.err); got != tc.want {
			t.Errorf("aiErrorClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
		if !slices.Contains(aiErrorClasses, tc.want) {
			t.Errorf("class %q is outside the closed set", tc.want)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, cancelled := callCancellation(ctx, tc.err); cancelled {
			t.Errorf("%v with the context done counted as a cancellation, want the provider's answer", tc.err)
		}
	}
}
