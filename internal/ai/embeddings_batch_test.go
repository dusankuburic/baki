package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// embedBatchServer answers /embeddings with one vector per input, echoing each input's numeric label as its
// single dimension so order survives; a request over maxInputs is refused with a realistic batch-limit 400.
// Returns the sizes of every request observed, in order.
func embedBatchServer(t *testing.T, maxInputs int) (*httptest.Server, func() []int) {
	t.Helper()
	var mu sync.Mutex
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sizes = append(sizes, len(body.Input))
		mu.Unlock()
		if len(body.Input) > maxInputs {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code":    "BadRequest",
				"message": "Too many input items in a single request. The maximum number of input items allowed is " + strconv.Itoa(maxInputs) + ".",
			}})
			return
		}
		out := make([]map[string]any, len(body.Input))
		for i, in := range body.Input {
			idx, _ := strconv.Atoi(in)
			out[i] = map[string]any{"embedding": []float32{float32(idx)}}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	t.Cleanup(srv.Close)
	return srv, func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), sizes...)
	}
}

// embedProviderFor points a plain OpenAI provider at an embeddings-capable
// test server.
func embedProviderFor(srv *httptest.Server) *OpenAIProvider {
	return compatErrProvider(srv.Client(), srv.URL+"/chat/completions")
}

// numberedInputs builds n inputs labeled with their index.
func numberedInputs(n int) []string {
	texts := make([]string, n)
	for i := range texts {
		texts[i] = strconv.Itoa(i)
	}
	return texts
}

// A resource capping /embeddings at 16 inputs must not fail indexing: the 40-input batch splits until every
// request fits, the full batch attempted FIRST, results in original order.
func TestEmbed_SplitsBatchOnInputCapAndPreservesOrder(t *testing.T) {
	srv, sizes := embedBatchServer(t, 16)
	p := embedProviderFor(srv)
	p.setEmbeddingModel("text-embedding-3-small")

	vecs, err := p.Embed(context.Background(), numberedInputs(40))
	if err != nil {
		t.Fatalf("Embed of 40 inputs against a 16-input cap: %v", err)
	}
	if len(vecs) != 40 {
		t.Fatalf("got %d vectors, want 40", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 1 || int(v[0]) != i {
			t.Fatalf("vector %d = %v, want [%d] (order not preserved across the splits)", i, v, i)
		}
	}
	// Depth-first halving of 40 under a 16 cap: 40 refused, its left half 20 refused, then that half's
	// 10|10 succeed before the right 20 and its 10|10 follow.
	want := []int{40, 20, 10, 10, 20, 10, 10}
	if got := sizes(); !equalInts(got, want) {
		t.Errorf("request sizes = %v, want %v (full batch first, then halves)", got, want)
	}
}

// A provider that accepts the full batch sees exactly ONE request — no pre-splitting.
func TestEmbed_AcceptingServerSingleRequest(t *testing.T) {
	srv, sizes := embedBatchServer(t, 1024)
	p := embedProviderFor(srv)
	p.setEmbeddingModel("text-embedding-3-small")

	if _, err := p.Embed(context.Background(), numberedInputs(40)); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := sizes(); !equalInts(got, []int{40}) {
		t.Errorf("request sizes = %v, want [40] (no pre-splitting)", got)
	}
}

// A server rejecting EVERY batch size (even a single input) must not loop: the split bottoms out at one input
// and the error surfaces after a bounded number of requests.
func TestEmbed_SplitBoundedAtSingleInput(t *testing.T) {
	srv, sizes := embedBatchServer(t, 0)
	p := embedProviderFor(srv)
	p.setEmbeddingModel("text-embedding-3-small")

	_, err := p.Embed(context.Background(), numberedInputs(2))
	if err == nil || !strings.Contains(err.Error(), "Too many input items") {
		t.Fatalf("want the batch-limit 400 surfaced, got %v", err)
	}
	if got := sizes(); !equalInts(got, []int{2, 1}) {
		t.Errorf("request sizes = %v, want [2 1] (the left single input's failure fails fast; the split stops at single inputs)", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEmbedBatchLimitRejection_CountOnly: the split trigger matches batch COUNT rejections only — a single
// oversized input cannot be shrunk by any split, so per-input SIZE rejections used to run a doomed cascade.
func TestEmbedBatchLimitRejection_CountOnly(t *testing.T) {
	for _, msg := range []string{
		"Too many input items in a single request. The maximum number of input items allowed is 16.",
		"maximum number of inputs",
		"the batch size exceeds the allowed maximum",
		"Input batch count limit exceeded",
	} {
		if !embedBatchLimitRejected([]byte(msg)) {
			t.Errorf("batch-count rejection not matched: %q", msg)
		}
	}
	for _, msg := range []string{
		"the input size exceeds the maximum allowed size of 3072 tokens",
		"maximum input size exceeded",
		"too many tokens in the input",
		"input token limit exceeded",
		// The count pattern matches the substring "too many input" inside
		// these; the token guard must keep them out of the split path.
		"too many input tokens",
		"too many input tokens exceeded",
		"input tokens exceed the maximum",
	} {
		if embedBatchLimitRejected([]byte(msg)) {
			t.Errorf("per-input size rejection must not trigger a split: %q", msg)
		}
	}
}

// TestEmbed_Leaf429HealsInCascadeWithoutOuterReRun: a transient leaf 429 used to fail the cascade AND make
// the outer RetryingProvider re-run it; the leaf now gets one local retry and a terminal error, so the
// request count is the cascade alone.
func TestEmbed_Leaf429HealsInCascadeWithoutOuterReRun(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sizes = append(sizes, len(body.Input))
		mu.Unlock()
		if len(body.Input) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code":    "BadRequest",
				"message": "Too many input items in a single request.",
			}})
			return
		}
		if len(body.Input) == 1 && body.Input[0] == "3" {
			// The doomed leaf: throttled forever.
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "Requests to the embeddings endpoint are being throttled."}})
			return
		}
		out := make([]map[string]any, len(body.Input))
		for i, in := range body.Input {
			idx, _ := strconv.Atoi(in)
			out[i] = map[string]any{"embedding": []float32{float32(idx)}}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	t.Cleanup(srv.Close)

	p := embedProviderFor(srv)
	p.setEmbeddingModel("text-embedding-3-small")
	rp := NewRetryingProviderWithConfig(p, 3, time.Millisecond)

	_, err := rp.Embed(context.Background(), numberedInputs(4))
	if err == nil {
		t.Fatal("expected the throttled leaf to fail the batch")
	}
	if errors.Is(err, ErrRateLimited) {
		t.Fatalf("the final error must not be retryable by the outer wrapper (it would re-run the cascade): %v", err)
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("the server's rejection must surface in the error, got: %v", err)
	}
	// Cascade: 4 rejected, its left 2 rejected then 1|1 ok, its right 2 rejected, leaf [2] ok, leaf [3]
	// throttled twice (local retry) — the outer wrapper adds nothing.
	want := []int{4, 2, 1, 1, 2, 1, 1, 1}
	if got := sizes; !equalInts(got, want) {
		t.Errorf("request sizes = %v, want %v (no outer re-run of the cascade)", got, want)
	}
}

// TestEmbed_Leaf429RecoversLocally: when the leaf's second attempt succeeds, the local retry heals the
// cascade — the transient error never leaves embed().
func TestEmbed_Leaf429RecoversLocally(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	throttled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sizes = append(sizes, len(body.Input))
		mu.Unlock()
		if len(body.Input) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code":    "BadRequest",
				"message": "Too many input items in a single request.",
			}})
			return
		}
		if len(body.Input) == 1 && body.Input[0] == "3" && !throttled {
			throttled = true // one transient blip on the leaf
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "throttled"}})
			return
		}
		out := make([]map[string]any, len(body.Input))
		for i, in := range body.Input {
			idx, _ := strconv.Atoi(in)
			out[i] = map[string]any{"embedding": []float32{float32(idx)}}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	t.Cleanup(srv.Close)

	p := embedProviderFor(srv)
	p.setEmbeddingModel("text-embedding-3-small")
	rp := NewRetryingProviderWithConfig(p, 3, time.Millisecond)

	vecs, err := rp.Embed(context.Background(), numberedInputs(4))
	if err != nil {
		t.Fatalf("a transient leaf 429 must heal inside the cascade: %v", err)
	}
	if len(vecs) != 4 || int(vecs[3][0]) != 3 {
		t.Errorf("vectors = %v, want all four in order", vecs)
	}
	want := []int{4, 2, 1, 1, 2, 1, 1, 1}
	if got := sizes; !equalInts(got, want) {
		t.Errorf("request sizes = %v, want %v", got, want)
	}
}

// TestEmbed_Leaf429WithHugeRetryAfterFailsFast: a leaf 429 with "Retry-After: 120" used to sleep two minutes
// per leaf, sequentially. The leaf retry refuses hints beyond embedLocalRetryMax: no second request, the
// terminal error surfaces at once.
func TestEmbed_Leaf429WithHugeRetryAfterFailsFast(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	leaf3 := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sizes = append(sizes, len(body.Input))
		if len(body.Input) == 1 && body.Input[0] == "3" {
			leaf3++
		}
		mu.Unlock()
		if len(body.Input) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code":    "BadRequest",
				"message": "Too many input items in a single request.",
			}})
			return
		}
		if len(body.Input) == 1 && body.Input[0] == "3" {
			// The doomed leaf: throttled with a huge hint.
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "Requests to the embeddings endpoint are being throttled."}})
			return
		}
		out := make([]map[string]any, len(body.Input))
		for i, in := range body.Input {
			idx, _ := strconv.Atoi(in)
			out[i] = map[string]any{"embedding": []float32{float32(idx)}}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	t.Cleanup(srv.Close)

	p := embedProviderFor(srv)
	p.setEmbeddingModel("text-embedding-3-small")
	rp := NewRetryingProviderWithConfig(p, 3, time.Millisecond)

	start := time.Now()
	_, err := rp.Embed(context.Background(), numberedInputs(4))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the throttled leaf to fail the batch")
	}
	if errors.Is(err, ErrRateLimited) {
		t.Fatalf("the final error must not be retryable by the outer wrapper (it would re-run the cascade): %v", err)
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("the server's rejection must surface in the error, got: %v", err)
	}
	mu.Lock()
	got, doomed := append([]int(nil), sizes...), leaf3
	mu.Unlock()
	// Same cascade as the heals test but WITHOUT the leaf's second request.
	want := []int{4, 2, 1, 1, 2, 1, 1}
	if !equalInts(got, want) {
		t.Errorf("request sizes = %v, want %v (the huge hint must not be slept on)", got, want)
	}
	if doomed != 1 {
		t.Errorf("doomed leaf received %d requests, want exactly 1 (retry refused on the huge hint)", doomed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Embed took %v; a Retry-After of 120s must fail fast, not sleep", elapsed)
	}
}

// TestEmbed_Leaf429WithShortRetryAfterStillRetries: a SHORT server hint stays on the local-retry path — the
// leaf waits it out and the second attempt completes the cascade.
func TestEmbed_Leaf429WithShortRetryAfterStillRetries(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	throttled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sizes = append(sizes, len(body.Input))
		mu.Unlock()
		if len(body.Input) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code":    "BadRequest",
				"message": "Too many input items in a single request.",
			}})
			return
		}
		if len(body.Input) == 1 && body.Input[0] == "3" && !throttled {
			throttled = true // one transient blip with a sub-cap hint
			w.Header().Set("retry-after-ms", "10")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "throttled"}})
			return
		}
		out := make([]map[string]any, len(body.Input))
		for i, in := range body.Input {
			idx, _ := strconv.Atoi(in)
			out[i] = map[string]any{"embedding": []float32{float32(idx)}}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	t.Cleanup(srv.Close)

	p := embedProviderFor(srv)
	p.setEmbeddingModel("text-embedding-3-small")
	rp := NewRetryingProviderWithConfig(p, 3, time.Millisecond)

	vecs, err := rp.Embed(context.Background(), numberedInputs(4))
	if err != nil {
		t.Fatalf("a leaf 429 with a short hint must heal inside the cascade: %v", err)
	}
	if len(vecs) != 4 || int(vecs[3][0]) != 3 {
		t.Errorf("vectors = %v, want all four in order", vecs)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []int{4, 2, 1, 1, 2, 1, 1, 1}
	if !equalInts(sizes, want) {
		t.Errorf("request sizes = %v, want %v (local retry after the short hint)", sizes, want)
	}
}

// TestEmbed_TokenSizeRejectionDoesNotCascade: a per-input SIZE rejection (token wording) must surface after
// ONE request — the count pattern matching a substring of it ("too many input" inside "too many input tokens
// exceeded") must not run the doomed 2n-1-request cascade.
func TestEmbed_TokenSizeRejectionDoesNotCascade(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code":    "BadRequest",
			"message": "Too many input tokens in a single input. The maximum allowed tokens per input is 8192.",
		}})
	}))
	t.Cleanup(srv.Close)

	p := embedProviderFor(srv)
	p.setEmbeddingModel("text-embedding-3-small")

	_, err := p.Embed(context.Background(), numberedInputs(8))
	if err == nil || !strings.Contains(err.Error(), "Too many input tokens") {
		t.Fatalf("want the size rejection surfaced, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Errorf("size rejection made %d requests, want 1 (no split cascade)", requests)
	}
}

// TestEmbed_SplitBudgetFailsWithServerError: a server rejecting every batch size cannot make the cascade
// exceed its request budget; the server's own rejection surfaces instead of more requests.
func TestEmbed_SplitBudgetFailsWithServerError(t *testing.T) {
	srv, sizes := embedBatchServer(t, 0)
	p := embedProviderFor(srv)
	p.setEmbeddingModel("text-embedding-3-small")

	_, err := p.Embed(context.Background(), numberedInputs(64))
	if err == nil || !strings.Contains(err.Error(), "Too many input items") {
		t.Fatalf("want the server's batch-limit rejection surfaced, got %v", err)
	}
	if got := len(sizes()); got > 4*64+8 {
		t.Errorf("cascade made %d requests, budget is %d", got, 4*64+8)
	}
}
