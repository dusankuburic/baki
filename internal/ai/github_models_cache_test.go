package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// resetGHModelsCacheForTest clears the process-wide GitHub Models listing
// cache so cases don't leak into each other.
func resetGHModelsCacheForTest() {
	ghModelsMu.Lock()
	ghModelsCached = nil
	ghModelsExpiry = time.Time{}
	ghModelsMu.Unlock()
}

// TestGitHubModels_ModelsCachedProcessWide proves the /models listing is
// fetched once and served from cache afterwards (it sits on the pricing +
// context-limit path, so every usage record used to re-GET it).
func TestGitHubModels_ModelsCachedProcessWide(t *testing.T) {
	resetGHModelsCacheForTest()
	t.Cleanup(resetGHModelsCacheForTest)

	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name": "gpt-4o", "context_limit": 8192}, {"name": "Phi-4", "context_limit": 16384}]`))
	}))
	defer srv.Close()

	prev := githubModelsBaseURL
	githubModelsBaseURL = srv.URL + "/chat/completions"
	defer func() { githubModelsBaseURL = prev }()

	p := NewGitHubModelsProvider("token")
	for i := 0; i < 3; i++ {
		models, err := p.Models(context.Background())
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if len(models) != 2 {
			t.Fatalf("call %d: want 2 models, got %d", i, len(models))
		}
	}
	// A FRESH instance (mirrors the per-request factory construction) must
	// also hit the cache — that's why it's process-wide.
	if _, err := NewGitHubModelsProvider("token").Models(context.Background()); err != nil {
		t.Fatalf("fresh instance: %v", err)
	}
	if n := gets.Load(); n != 1 {
		t.Errorf("want exactly 1 live /models GET across 4 calls + 2 instances, got %d", n)
	}
}

// TestGitHubModels_FallbackNotCached proves a failed listing (5xx → catalog
// fallback) is not pinned for the TTL: the next call retries the wire.
func TestGitHubModels_FallbackNotCached(t *testing.T) {
	resetGHModelsCacheForTest()
	t.Cleanup(resetGHModelsCacheForTest)

	var gets atomic.Int32
	fail := atomic.Bool{}
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name": "gpt-4o", "context_limit": 8192}]`))
	}))
	defer srv.Close()

	prev := githubModelsBaseURL
	githubModelsBaseURL = srv.URL + "/chat/completions"
	defer func() { githubModelsBaseURL = prev }()

	p := NewGitHubModelsProvider("token")
	models, err := p.Models(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatalf("fallback call: want catalog fallback, got %v / %d models", err, len(models))
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("setup: want 1 GET, got %d", n)
	}

	fail.Store(false)
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n := gets.Load(); n != 2 {
		t.Errorf("fallback must not be cached — want a retry GET (2 total), got %d", n)
	}
}
