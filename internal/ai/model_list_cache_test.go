package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/egress"
)

// copilotModelsServer serves a fixed Copilot /models listing (or status
// when non-zero) and counts the requests.
func copilotModelsServer(t *testing.T, status *atomic.Int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if status != nil && status.Load() != 0 {
			w.WriteHeader(int(status.Load()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o","name":"GPT-4o","context_limit":128000},{"id":"o3","name":"o3","context_limit":200000}]}`))
	}))
	t.Cleanup(srv.Close)
	orig := copilotModelsURL
	copilotModelsURL = srv.URL + "/models"
	t.Cleanup(func() { copilotModelsURL = orig })
	return srv, &hits
}

// The factory builds a fresh CopilotProvider per request and chat reads the
// listing before every turn: the listing must be fetched once per
// credential, not once per provider instance (one HTTPS round trip ahead of
// every Copilot turn's first token).
func TestCopilotModels_CachedAcrossProviderInstances(t *testing.T) {
	_, hits := copilotModelsServer(t, nil)

	for turn := 0; turn < 5; turn++ {
		p := NewCopilotProvider("pat-cache-a") // a fresh provider per turn, as forCopilot builds it
		models, err := p.Models(context.Background())
		if err != nil || len(models) != 2 {
			t.Fatalf("turn %d: Models = %+v, %v", turn, models, err)
		}
		models[0].ID = "mutated" // the caller's copy: must not reach the cache
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("listing fetched %d times across 5 provider instances, want 1", n)
	}
	if got, _ := NewCopilotProvider("pat-cache-a").Models(context.Background()); got[0].ID != "gpt-4o" {
		t.Errorf("cached listing was mutated through a returned slice: %+v", got)
	}

	// Another credential is another subscription: its own fetch.
	if _, err := NewCopilotProvider("pat-cache-b").Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("a second credential shared the first one's listing (hits=%d, want 2)", n)
	}

	// The key holds a hash of the credential, never the token itself.
	copilotModels.mu.Lock()
	for k := range copilotModels.m {
		if strings.Contains(k, "pat-cache-a") || strings.Contains(k, "pat-cache-b") {
			t.Errorf("cache key carries the raw token: %q", k)
		}
	}
	copilotModels.mu.Unlock()
}

// A failed listing is cached only briefly: the next turns inside the window
// do not hammer a failing endpoint, and the first one after it retries.
func TestCopilotModels_ErrorCachedBriefly(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusBadGateway)
	_, hits := copilotModelsServer(t, &status)
	origTTL := copilotModelsErrCacheTTL
	copilotModelsErrCacheTTL = 150 * time.Millisecond
	t.Cleanup(func() { copilotModelsErrCacheTTL = origTTL })

	for i := 0; i < 3; i++ {
		if _, err := NewCopilotProvider("pat-err").Models(context.Background()); err == nil {
			t.Fatal("a 502 listing must fail")
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("failed listing fetched %d times inside the error window, want 1", n)
	}
	status.Store(0)
	time.Sleep(200 * time.Millisecond)
	models, err := NewCopilotProvider("pat-err").Models(context.Background())
	if err != nil || len(models) != 2 {
		t.Fatalf("after the error window: %+v, %v (want the live listing)", models, err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("hits = %d, want 2 (one retry after the error window)", n)
	}
}

// The catalog fallback (no session token) is never cached: the next call
// with a working token must fetch the live listing.
func TestCopilotModels_FallbackNotCached(t *testing.T) {
	_, hits := copilotModelsServer(t, nil)
	token := ""
	p := &CopilotProvider{openaiBase: openaiBase{tokenFn: func(context.Context) (string, error) { return token, nil }, client: sharedHTTPClient, baseURL: &copilotBaseURL}, modelsKey: "oauth:fallback-test"}
	if models, err := p.Models(context.Background()); err != nil || len(models) == 0 {
		t.Fatalf("fallback: %+v, %v", models, err)
	}
	token = "session"
	if models, err := p.Models(context.Background()); err != nil || len(models) != 2 {
		t.Fatalf("live after fallback: %+v, %v", models, err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("hits = %d, want 1", n)
	}
}

// The local server's listing is cached briefly by base URL: one GET for
// several per-request provider instances, each entry still carrying the
// instance's own configured window.
func TestLocalModels_CachedAcrossProviderInstances(t *testing.T) {
	saveLocalModelsPolicy(t, egress.AllowLoopback)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "qwen2.5:7b"}, {"id": "nomic-embed-text"}}})
	}))
	t.Cleanup(srv.Close)

	for i, window := range []int{0, 32768, 0} {
		doc := `{"baseUrl":"` + srv.URL + `"}`
		want := LocalDefaultContextWindow
		if window > 0 {
			doc = `{"baseUrl":"` + srv.URL + `","contextWindow":32768}`
			want = 32768
		}
		models, err := NewLocalProvider(doc).Models(context.Background())
		if err != nil || len(models) != 1 || models[0].ID != "qwen2.5:7b" {
			t.Fatalf("call %d: %+v, %v", i, models, err)
		}
		if models[0].ContextLimit != want {
			t.Errorf("call %d: ContextLimit = %d, want %d", i, models[0].ContextLimit, want)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("local listing fetched %d times, want 1", n)
	}

	// Saving the configuration reads the listing again (a model just pulled
	// into the server shows up at once).
	if _, err := NewLocalConfigForSave(srv.URL, 0); err != nil {
		t.Fatalf("NewLocalConfigForSave: %v", err)
	}
	if _, err := NewLocalProvider(`{"baseUrl":"` + srv.URL + `"}`).Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("hits = %d after a configuration save, want 2", n)
	}
}

// A stopped local server is reported at once, every time: failures are not
// cached.
func TestLocalModels_ErrorNotCached(t *testing.T) {
	saveLocalModelsPolicy(t, egress.AllowLoopback)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	for i := 0; i < 2; i++ {
		if _, err := NewLocalProvider(`{"baseUrl":"` + srv.URL + `"}`).Models(context.Background()); err == nil {
			t.Fatal("a 503 listing must fail")
		}
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("hits = %d, want 2 (errors are not cached)", n)
	}
}

// The listing cache stays bounded, evicting the soonest-expiring entry.
func TestModelListCache_Bounded(t *testing.T) {
	c := newModelListCache(2)
	fetch := func(context.Context) ([]ModelInfo, bool, error) { return []ModelInfo{{ID: "m"}}, true, nil }
	for i, ttl := range []time.Duration{time.Minute, time.Hour, time.Hour} {
		if _, err := c.load(context.Background(), string(rune('a'+i)), ttl, 0, fetch); err != nil {
			t.Fatal(err)
		}
	}
	if c.size() != 2 {
		t.Fatalf("size = %d, want 2", c.size())
	}
	if _, _, ok := c.get("a"); ok {
		t.Error("the soonest-expiring entry should have been evicted")
	}
}

// A forget (the local config was saved: "read the listing again") while a
// fetch is in flight must win: the fetch that started before it used to store
// its stale listing afterwards, and loads in between joined that old flight.
func TestModelListCache_ForgetDuringFetchIsNotUndone(t *testing.T) {
	c := newModelListCache(8)
	started, release := make(chan struct{}), make(chan struct{})
	oldFetch := func(context.Context) ([]ModelInfo, bool, error) {
		close(started)
		<-release
		return []ModelInfo{{ID: "old"}}, true, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.load(context.Background(), "k", time.Hour, 0, oldFetch)
	}()
	<-started
	c.forget("k")

	newFetch := func(context.Context) ([]ModelInfo, bool, error) { return []ModelInfo{{ID: "new"}}, true, nil }
	// Bounded: joining the old flight would otherwise wait on release forever.
	jctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	got, err := c.load(jctx, "k", time.Hour, 0, newFetch)
	cancel()
	if err != nil || len(got) != 1 || got[0].ID != "new" {
		t.Errorf("a load after forget joined the old fetch: got %v, %v", got, err)
	}
	close(release)
	<-done
	got, err = c.load(context.Background(), "k", time.Hour, 0, newFetch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "new" {
		t.Errorf("the fetch begun before forget re-cached its stale listing: got %v", got)
	}
}

// Every chain the factory builds may embed concurrently (the knowledge
// base overlaps its upload batches only for those) and forwards the raw
// provider's embedding identity through the wrappers.
func TestFactoryChain_EmbedsConcurrentlyAndNamesIdentity(t *testing.T) {
	f := NewProviderFactory(func(_, _ string) (string, error) { return "sk-test", nil }, nil, nil, nil)
	p, err := f.ForEmbedding("u1", "openai", "text-embedding-3-small")
	if err != nil {
		t.Fatal(err)
	}
	if !EmbedsConcurrently(p) {
		t.Error("a factory chain must report concurrent embedding")
	}
	if id := EmbeddingIdentity(p); !strings.Contains(id, "text-embedding-3-small") {
		t.Errorf("identity = %q, want the embedding model in it", id)
	}
	other, _ := f.ForEmbedding("u1", "openai", "text-embedding-3-large")
	if EmbeddingIdentity(other) == EmbeddingIdentity(p) {
		t.Error("two embedding models share one identity")
	}
	if EmbedsConcurrently(&auditStub{}) || EmbeddingIdentity(&auditStub{}) != "" {
		t.Error("a bare provider must not claim concurrency or an identity")
	}
}

// GetSessionToken used to sweep the whole session-token map under its mutex
// on every call; the sweep now runs at most once per interval.
func TestCopilotAuth_SweepThrottled(t *testing.T) {
	a := NewCopilotAuth()
	now := time.Now().Unix()
	a.sessionTokens[tokenHash("gh-live")] = &CopilotSessionToken{Token: "session", ExpiresAt: now + 3600}
	a.sessionTokens["stale-1"] = &CopilotSessionToken{Token: "old", ExpiresAt: now - 10}

	if tok, err := a.GetSessionToken(context.Background(), "gh-live"); err != nil || tok != "session" {
		t.Fatalf("GetSessionToken = %q, %v", tok, err)
	}
	if _, ok := a.sessionTokens["stale-1"]; ok {
		t.Fatal("the first call must sweep expired entries")
	}

	a.sessionTokens["stale-2"] = &CopilotSessionToken{Token: "old", ExpiresAt: now - 10}
	for i := 0; i < 3; i++ {
		if _, err := a.GetSessionToken(context.Background(), "gh-live"); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := a.sessionTokens["stale-2"]; !ok {
		t.Fatal("calls inside the sweep interval must not walk the map again")
	}

	a.mu.Lock()
	a.lastSweep = time.Now().Add(-copilotTokenSweepInterval - time.Second)
	a.mu.Unlock()
	if _, err := a.GetSessionToken(context.Background(), "gh-live"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.sessionTokens["stale-2"]; ok {
		t.Error("the first call after the interval must sweep again")
	}
}
