package ai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// Model listing with Entra ID and a project endpoint (findings #1, #20, #24,
// #28, #42). The API-key listing tests live in azure_foundry_test.go.

func TestAzureFoundry_ModelsIncludeProjectDeploymentsAndAgents(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			if r.URL.Query().Get("api-version") != "v1" {
				t.Errorf("deployments api-version = %q", r.URL.Query().Get("api-version"))
			}
			writeJSON(w, 200, map[string]any{"value": []map[string]any{
				{"name": "chat1", "type": "ModelDeployment", "modelName": "gpt-4o", "modelPublisher": "OpenAI", "capabilities": map[string]any{"chat_completion": "true"}},
				{"name": "emb1", "type": "ModelDeployment", "modelName": "text-embedding-3-small", "capabilities": map[string]any{"embeddings": "true"}},
				{"name": "extra", "type": "ModelDeployment", "modelName": "DeepSeek-V3-0324"},
				{"name": "vec", "type": "ModelDeployment", "modelName": "Cohere-v3-english", "capabilities": map[string]any{"embeddings": true}},
			}})
		case "/api/projects/p1/agents":
			// Both the first page and the after= continuation ask for
			// the List Agents maximum page size, not the default 20.
			if got := r.URL.Query().Get("limit"); got != "100" {
				t.Errorf("agents listing limit = %q, want 100", got)
			}
			if r.URL.Query().Get("after") != "" {
				writeJSON(w, 200, map[string]any{"data": []map[string]any{{"name": "Second"}}, "has_more": false})
				return
			}
			writeJSON(w, 200, map[string]any{"data": []map[string]any{
				{"id": "agent_1", "name": "Helper", "versions": map[string]any{"latest": map[string]any{"definition": map[string]any{"kind": "prompt", "model": "gpt-4.1"}}}},
			}, "has_more": true, "last_id": "agent_1"})
		default:
			// Never GET /openai/v1/models: it is the model catalogue.
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "chat1"
	p.cfg.EmbeddingDeployment = "emb1"
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	var ids []string
	byID := map[string]ModelInfo{}
	for _, m := range models {
		ids = append(ids, m.ID)
		byID[m.ID] = m
	}
	if got := strings.Join(ids, ","); got != "chat1,emb1,extra,vec,agent:Helper,agent:Second" {
		t.Errorf("listing = %s, want the document's deployments, the project's others in listing order, then the agents", got)
	}
	if m := byID["chat1"]; m.DisplayName != "chat1 · gpt-4o" || m.ServedModel != "gpt-4o" || m.Embedding {
		t.Errorf("chat1 = %+v", m)
	}
	if m := byID["emb1"]; !m.Embedding || m.ServedModel != "text-embedding-3-small" {
		t.Errorf("emb1 = %+v, want the embedding flag and its model", m)
	}
	if m, ok := byID["extra"]; !ok || m.ServedModel != "DeepSeek-V3-0324" || m.Embedding {
		t.Errorf("project-only deployment missing/unenriched: %+v", m)
	}
	if m := byID["vec"]; !m.Embedding {
		t.Errorf("project deployment whose capabilities say embeddings = %+v, want the embedding flag", m)
	}
	if m := byID["agent:Helper"]; m.DisplayName != "Agent · Helper · gpt-4.1" || m.ServedModel != "gpt-4.1" {
		t.Errorf("agent entry = %+v", m)
	}

	// A complete listing is cached for the document.
	before := len(srv.requests())
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := len(srv.requests()); after != before {
		t.Errorf("second listing made %d requests, want a cache hit", after-before)
	}
}

// entraProjectServer serves a project whose deployments listing answers
// with deployments (status 200) or the given status, and whose agents
// listing is driven by agents(n) — n counts the agents requests.
func entraProjectServer(t *testing.T, deploymentsStatus int, agents func(n int, w http.ResponseWriter)) (*azureTestServer, *int32) {
	t.Helper()
	var agentCalls int32
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			if deploymentsStatus != 200 {
				writeJSON(w, deploymentsStatus, map[string]any{"error": map[string]any{"code": "Denied", "message": "denied"}})
				return
			}
			writeJSON(w, 200, map[string]any{"value": []map[string]any{}})
		case "/api/projects/p1/agents":
			agents(int(atomic.AddInt32(&agentCalls, 1)), w)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	return srv, &agentCalls
}

func helperAgents(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{"data": []map[string]any{{"name": "Helper", "definition": map[string]any{"kind": "prompt"}}}})
}

func hasModel(models []ModelInfo, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// expireAzureListing back-dates the cached listing for cfg so the next
// Models call asks the project again.
func expireAzureListing(t *testing.T, cfg AzureFoundryConfig) {
	t.Helper()
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	e, ok := azureModelsCache.m[azureModelsCacheKey(cfg)]
	if !ok {
		t.Fatal("no cached listing to expire")
	}
	e.expires, e.retryAt = time.Now().Add(-time.Second), time.Time{}
	azureModelsCache.m[azureModelsCacheKey(cfg)] = e
}

// a listing cut short by a transient project failure is
// served for at most azureModelsRetryAfter — not re-fetched on every turn
// (up to the 10 s extras bound each) while the failure lasts, and never
// hidden for the full TTL — then asked again; a definitive refusal (403 on
// the deployments route) still counts as complete.
func TestAzureFoundry_IncompleteProjectListingIsServedBriefly(t *testing.T) {
	srv, calls := entraProjectServer(t, 200, func(n int, w http.ResponseWriter) {
		if n == 1 {
			writeJSON(w, 503, map[string]any{"error": map[string]any{"message": "busy"}})
			return
		}
		helperAgents(w)
	})
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "chat1"
	first, err := p.Models(context.Background())
	if err != nil || !hasModel(first, "chat1") || hasModel(first, "agent:Helper") {
		t.Fatalf("first listing = %+v, %v; want the configured deployment without agents", first, err)
	}
	azureModelsCache.mu.Lock()
	entry, cached := azureModelsCache.m[azureModelsCacheKey(p.cfg)]
	azureModelsCache.mu.Unlock()
	if !cached {
		t.Fatal("an incomplete listing must be cached briefly")
	}
	if ttl := time.Until(entry.expires); ttl > azureModelsRetryAfter || ttl <= 0 {
		t.Errorf("incomplete listing cached for %v, want at most %v", ttl, azureModelsRetryAfter)
	}
	second, _ := p.Models(context.Background())
	if hasModel(second, "agent:Helper") || atomic.LoadInt32(calls) != 1 {
		t.Errorf("within %v the incomplete listing is served as is: agents calls=%d listing=%+v", azureModelsRetryAfter, atomic.LoadInt32(calls), second)
	}
	expireAzureListing(t, p.cfg)
	third, _ := p.Models(context.Background())
	if !hasModel(third, "agent:Helper") || atomic.LoadInt32(calls) != 2 {
		t.Errorf("after the retry window the listing must ask again: agents calls=%d listing=%+v", atomic.LoadInt32(calls), third)
	}
	_, _ = p.Models(context.Background())
	if atomic.LoadInt32(calls) != 2 {
		t.Errorf("the complete listing must be cached: agents calls=%d", atomic.LoadInt32(calls))
	}
	azureModelsCache.mu.Lock()
	entry = azureModelsCache.m[azureModelsCacheKey(p.cfg)]
	azureModelsCache.mu.Unlock()
	if ttl := time.Until(entry.expires); ttl <= azureModelsRetryAfter {
		t.Errorf("a complete listing keeps the full TTL, got %v", ttl)
	}

	// 403 on the deployments route (missing role) is an answer, not an outage.
	srv2, calls2 := entraProjectServer(t, 403, func(_ int, w http.ResponseWriter) { helperAgents(w) })
	q := newAzureAgentProvider(t, srv2)
	q.cfg.ChatDeployment = "chat1"
	if got, _ := q.Models(context.Background()); !hasModel(got, "agent:Helper") {
		t.Errorf("listing = %+v", got)
	}
	_, _ = q.Models(context.Background())
	if n := len(srv2.requests()); n != 2 || atomic.LoadInt32(calls2) != 1 {
		t.Errorf("a definitive 403 must not defeat the cache: %d requests", n)
	}
}

// a caller whose own context is cancelled gets the
// document's deployments at once and never poisons the cache with a listing
// cut by its deadline; the shared flight it started completes detached and
// serves the next caller (one agents call in total).
func TestAzureFoundry_CancelledListingIsNotCached(t *testing.T) {
	srv, calls := entraProjectServer(t, 200, func(_ int, w http.ResponseWriter) { helperAgents(w) })
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "chat1"
	if _, err := p.tokens.Token(context.Background()); err != nil { // warm the token source
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := p.Models(ctx)
	if err != nil || !hasModel(got, "chat1") || hasModel(got, "agent:Helper") {
		t.Errorf("cancelled listing = %+v, %v; want the configured deployment only", got, err)
	}
	got, _ = p.Models(context.Background())
	if !hasModel(got, "agent:Helper") || atomic.LoadInt32(calls) != 1 {
		t.Errorf("the cancelled listing was cached: agents calls=%d listing=%+v", atomic.LoadInt32(calls), got)
	}
}

// a project endpoint that hangs costs the listing at most the
// extras bound; the configured deployments are still listed.
func TestAzureFoundry_ProjectExtrasAreBounded(t *testing.T) {
	prev := azureProjectExtrasTimeout
	azureProjectExtrasTimeout = 150 * time.Millisecond
	t.Cleanup(func() { azureProjectExtrasTimeout = prev })
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "chat1"
	start := time.Now()
	got, err := p.Models(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("listing took %v, want about the extras bound", elapsed)
	}
	if err != nil || len(got) != 1 || got[0].ID != "chat1" {
		t.Errorf("listing = %+v, %v; want the configured deployment", got, err)
	}
}

// failingCred never issues a token and counts the attempts.
type failingCred struct{ calls int32 }

func (f *failingCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	atomic.AddInt32(&f.calls, 1)
	return azcore.AccessToken{}, errors.New("AADSTS7000215: Invalid client secret provided")
}

// after the token fetch failed the project extras are skipped —
// no second and third token attempt, no project request.
func TestAzureFoundry_ProjectExtrasSkippedAfterTokenFailure(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		t.Errorf("unexpected request %s", r.URL.Path)
	})
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "chat1"
	cred := &failingCred{}
	p.tokens = newEntraTokenSource(cred)
	got, err := p.Models(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "chat1" {
		t.Errorf("listing = %+v, %v; want the configured deployment", got, err)
	}
	if n := atomic.LoadInt32(&cred.calls); n != 1 {
		t.Errorf("token requested %d times, want once", n)
	}
	if _, err := p.tokens.Token(context.Background()); !errors.Is(err, errEntraToken) {
		t.Errorf("a token failure must carry errEntraToken: %v", err)
	}
}

// the cache key covers the deployment names, so two users of one
// resource and credential never see each other's deployments.
func TestAzureFoundry_ListingCacheIsPerDocument(t *testing.T) {
	srv, _ := entraProjectServer(t, 200, func(_ int, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{"data": []map[string]any{}})
	})
	a := newAzureAgentProvider(t, srv)
	a.cfg.ChatDeployment = "a-gpt5"
	b := newAzureFoundryProvider(a.cfg, nil, srv.Client())
	b.tokens, b.tokenFn = a.tokens, b.entraToken
	b.cfg.ChatDeployment = "b-gpt41mini"

	if got, _ := a.Models(context.Background()); len(got) != 1 || got[0].ID != "a-gpt5" {
		t.Errorf("A = %+v", got)
	}
	if got, _ := b.Models(context.Background()); len(got) != 1 || got[0].ID != "b-gpt41mini" {
		t.Errorf("B = %+v, want B's own deployment, not A's cached listing", got)
	}
	n := len(srv.requests())
	_, _ = a.Models(context.Background())
	_, _ = b.Models(context.Background())
	if len(srv.requests()) != n {
		t.Error("each document's listing must be cached under its own key")
	}
	if azureModelsCacheKey(a.cfg) == azureModelsCacheKey(b.cfg) || a.cfg.Fingerprint() != b.cfg.Fingerprint() {
		t.Error("cache key must differ by deployment names while the credential fingerprint does not")
	}
	InvalidateAzureFoundryModels(b.cfg)
	_, _ = b.Models(context.Background())
	if len(srv.requests()) == n {
		t.Error("InvalidateAzureFoundryModels must drop the document's listing (same key)")
	}
}

// A project 401 gets one fresh token: a revoked cached token recovers at
// once, while a principal without a project role (a fresh token refused too)
// is a definitive answer — the listing without project data is cached rather
// than re-asked on every chat turn, and the agents route is not tried.
func TestAzureFoundry_Project401RetriesOnceThenIsDefinitive(t *testing.T) {
	t.Run("revoked token recovers", func(t *testing.T) {
		srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
			if r.Header.Get("Authorization") == "Bearer tok-1" {
				writeJSON(w, 401, map[string]any{"error": map[string]any{"code": "Unauthorized", "message": "token revoked"}})
				return
			}
			switch r.URL.Path {
			case "/api/projects/p1/deployments":
				writeJSON(w, 200, map[string]any{"value": []map[string]any{}})
			case "/api/projects/p1/agents":
				helperAgents(w)
			}
		})
		p := newAzureAgentProvider(t, srv)
		p.cfg.ChatDeployment = "chat1"
		if got, _ := p.Models(context.Background()); !hasModel(got, "agent:Helper") {
			t.Errorf("listing = %+v, want the agents after the fresh-token retry", got)
		}
	})

	t.Run("principal without a project role", func(t *testing.T) {
		srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"code": "PermissionDenied", "message": "missing role"}})
		})
		p := newAzureAgentProvider(t, srv)
		p.cfg.ChatDeployment = "chat1"
		got, err := p.Models(context.Background())
		if err != nil || len(got) != 1 || got[0].ID != "chat1" {
			t.Errorf("listing = %+v, %v; want the configured deployment", got, err)
		}
		// The two routes are asked concurrently; each gets its first try and
		// one fresh-token retry, never more.
		deployments := 0
		for _, r := range srv.requests() {
			if r.Path == "/api/projects/p1/deployments" {
				deployments++
			}
		}
		if n := len(srv.requests()); deployments != 2 || n > 4 {
			t.Errorf("%d deployments requests of %d, want the first try and one fresh-token retry per route", deployments, n)
		}
		n := len(srv.requests())
		_, _ = p.Models(context.Background())
		if len(srv.requests()) != n {
			t.Errorf("a definitive 401 must be cached: %d requests after the second listing", len(srv.requests()))
		}
	})
}

// concurrent cold-cache listings of one document share one flight —
// one deployments GET and one agents GET — and every caller gets the result;
// the two project routes are fetched concurrently.
func TestAzureFoundry_ConcurrentColdListingsShareOneFlight(t *testing.T) {
	var deploymentsCalls atomic.Int32
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		time.Sleep(60 * time.Millisecond)
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			deploymentsCalls.Add(1)
			writeJSON(w, 200, map[string]any{"value": []map[string]any{{"name": "extra", "modelName": "gpt-4o"}}})
		case "/api/projects/p1/agents":
			helperAgents(w)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "chat1"
	if _, err := p.tokens.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var wg sync.WaitGroup
	results := make([][]ModelInfo, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := p.Models(context.Background())
			if err != nil {
				t.Error(err)
			}
			results[i] = got
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	for i, got := range results {
		if !hasModel(got, "agent:Helper") || !hasModel(got, "extra") || !hasModel(got, "chat1") {
			t.Errorf("caller %d listing = %+v, want the shared complete listing", i, got)
		}
	}
	if n := deploymentsCalls.Load(); n != 1 {
		t.Errorf("deployments GETs = %d, want 1 for 8 concurrent cold callers", n)
	}
	if n := len(srv.requests()); n != 2 {
		t.Errorf("project requests = %d, want exactly one per route", n)
	}
	// Both routes ran at once: about one 60 ms hop, not two.
	if elapsed > 150*time.Millisecond {
		t.Errorf("listing took %v, want the two project calls to overlap", elapsed)
	}
	// Each caller got its own copy of the listing.
	results[0][0].ID = "mutated"
	if results[1][0].ID == "mutated" {
		t.Error("callers must not share the listing's backing array")
	}
}

// a body cut short on a 200 (connection reset, deadline) is reported
// as the read error it is — not "parse deployments" — and stays transient.
func TestAzureFoundry_ProjectBodyReadErrorIsTransient(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		if r.URL.Path != "/api/projects/p1/deployments" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"value":[{"name":"chat1"`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	})
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "chat1"
	_, err := p.fetchProjectDeployments(context.Background())
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want the read error (unexpected EOF), got %v", err)
	}
	if strings.Contains(err.Error(), "parse deployments") {
		t.Errorf("a cut body must not read as a parse problem: %v", err)
	}
	if definitiveProjectFailure(err) {
		t.Error("a read error is transient, not a definitive answer")
	}
	got, complete := p.projectExtras(context.Background(), p.configuredDeployments())
	if complete || !hasModel(got, "chat1") {
		t.Errorf("extras = %+v complete=%v; want the configured deployment, incomplete", got, complete)
	}

	// Over the cap: explicit, not a silent truncation.
	big := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"value":[{"name":"`))
		_, _ = w.Write(bytes.Repeat([]byte("x"), azureProjectMaxBody))
		_, _ = w.Write([]byte(`"}]}`))
	})
	q := newAzureAgentProvider(t, big)
	if _, err := q.fetchProjectDeployments(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeded 4 MB") {
		t.Errorf("want an explicit over-limit error, got %v", err)
	}
}

// a 429 on the deployments route
// abandons the agents fetch — the project asked for back-off and the agents
// route shares its quota — instead of paging on; the listing is incomplete
// and the configured deployments are still returned.
func TestAzureFoundry_ProjectThrottleSkipsAgents(t *testing.T) {
	var agentCalls atomic.Int32
	agentsCancelled := make(chan struct{}, 1)
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			w.Header().Set("Retry-After", "30")
			writeJSON(w, 429, map[string]any{"error": map[string]any{"code": "429", "message": "throttled"}})
		case "/api/projects/p1/agents":
			agentCalls.Add(1)
			// Held open: the fetch must be cancelled, not answered and paged.
			select {
			case <-r.Context().Done():
				agentsCancelled <- struct{}{}
			case <-time.After(3 * time.Second):
				helperAgents(w)
			}
		}
	})
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "chat1"
	start := time.Now()
	got, complete := p.projectExtras(context.Background(), p.configuredDeployments())
	if complete || !hasModel(got, "chat1") || hasModel(got, "agent:Helper") {
		t.Errorf("extras = %+v complete=%v; want the configured deployment only, incomplete", got, complete)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the agents fetch was not abandoned on the 429: took %v", elapsed)
	}
	select {
	case <-agentsCancelled:
	case <-time.After(2 * time.Second):
		t.Error("the in-flight agents request was not cancelled")
	}
	if n := agentCalls.Load(); n > 1 {
		t.Errorf("agents route hit %d times, want at most the concurrent first request", n)
	}
}

// one malformed item costs that entry, not the page — the listing is
// complete and cached instead of being re-fetched on every turn for as long
// as the deployment exists.
func TestAzureFoundry_MalformedDeploymentItemIsSkipped(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			writeJSON(w, 200, map[string]any{"value": []any{
				map[string]any{"name": "odd", "modelName": "future-kind", "capabilities": []string{"chat"}},
				map[string]any{"name": "fine", "modelName": "gpt-4o", "capabilities": map[string]any{"chat_completion": "true"}},
			}})
		case "/api/projects/p1/agents":
			writeJSON(w, 200, map[string]any{"data": []any{
				42,
				map[string]any{"name": "Helper", "definition": map[string]any{"kind": "prompt"}},
			}})
		}
	})
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "chat1"
	got, err := p.Models(context.Background())
	if err != nil || !hasModel(got, "fine") || hasModel(got, "odd") || !hasModel(got, "agent:Helper") {
		t.Fatalf("listing = %+v, %v; want the well-formed items only", got, err)
	}
	n := len(srv.requests())
	_, _ = p.Models(context.Background())
	if len(srv.requests()) != n {
		t.Error("a listing with a skipped item is complete and must be cached")
	}
}

// D-39: an agent's definition names the PROJECT DEPLOYMENT it runs on; the
// deployments listing resolves the model behind it (window, output cap,
// price), a catalogued name resolves through the catalogue, and an unknown
// one leaves ServedModel empty so the served-model memo can fill it in once
// the agent has answered.
func TestAzureFoundry_AgentOnCustomDeploymentResolvesItsModel(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			writeJSON(w, 200, map[string]any{"value": []map[string]any{{"name": "prod-chat", "modelName": "gpt-4.1"}}})
		case "/api/projects/p1/agents":
			writeJSON(w, 200, map[string]any{"data": []map[string]any{
				{"name": "triage", "versions": map[string]any{"latest": map[string]any{"definition": map[string]any{"kind": "prompt", "model": "prod-chat"}}}},
				{"name": "mystery", "versions": map[string]any{"latest": map[string]any{"definition": map[string]any{"kind": "prompt", "model": "other-dep"}}}},
				{"name": "named", "versions": map[string]any{"latest": map[string]any{"definition": map[string]any{"kind": "prompt", "model": "Phi-4"}}}},
			}})
		}
	})
	p := newAzureAgentProvider(t, srv)
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if m := byID["agent:triage"]; m.ServedModel != "gpt-4.1" || m.ContextLimit != 1047576 || m.MaxOutputTokens != 32768 ||
		m.DisplayName != "Agent · triage · gpt-4.1" {
		t.Errorf("agent on a custom-named deployment = %+v, want the deployment's model", m)
	}
	if m := byID["agent:named"]; m.ServedModel != "Phi-4" || m.ContextLimit != 16384 {
		t.Errorf("agent on a catalogued deployment name = %+v", m)
	}
	if m := byID["agent:mystery"]; m.ServedModel != "" || m.ContextLimit != p.ContextLimit() || m.DisplayName != "Agent · mystery" {
		t.Errorf("agent on an unknown deployment = %+v, want an empty served model for the memo to fill", m)
	}

	// Once the agent answered, the memo enriches the entry.
	scope := "u1\x00" + p.servedMemoResource()
	recordServedModel(scope, azureFoundryProviderID, "agent:mystery", "gpt-4.1-2025-04-14")
	enriched := enrichServedModels(scope, azureFoundryProviderID, models)
	for _, m := range enriched {
		if m.ID == "agent:mystery" && (m.ServedModel != "gpt-4.1-2025-04-14" || m.ContextLimit != 1047576) {
			t.Errorf("memo did not enrich the agent: %+v", m)
		}
	}
}

// The deployments listing follows nextLink continuation pages — a
// regression that stops following them silently hides every deployment
// beyond the first page. The page-1 nextLink is built from the test
// server's own https URL (the continuation guard only follows same-host
// https links).
func TestAzureFoundry_ProjectDeploymentsFollowNextLink(t *testing.T) {
	var srv *azureTestServer
	srv = newAzureTLSTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			if r.URL.Query().Get("skipToken") != "" {
				// Page 2: the continuation the page-1 nextLink asked for.
				if r.URL.Query().Get("skipToken") != "2" {
					t.Errorf("page 2 skipToken = %q, want 2", r.URL.Query().Get("skipToken"))
				}
				writeJSON(w, 200, map[string]any{"value": []map[string]any{
					{"name": "page2-phi4", "modelName": "Phi-4"},
				}})
				return
			}
			writeJSON(w, 200, map[string]any{
				"value": []map[string]any{
					{"name": "page1-gpt4o", "modelName": "gpt-4o", "modelPublisher": "OpenAI", "capabilities": map[string]any{"chat_completion": "true"}},
				},
				"nextLink": srv.URL + "/api/projects/p1/deployments?api-version=v1&skipToken=2",
			})
		case "/api/projects/p1/agents":
			writeJSON(w, 200, map[string]any{"data": []map[string]any{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newAzureAgentProvider(t, srv)
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if !hasModel(models, "page1-gpt4o") || !hasModel(models, "page2-phi4") {
		t.Errorf("listing = %+v, want the deployments of both pages", models)
	}
	deployments := 0
	for _, r := range srv.requests() {
		if r.Path == "/api/projects/p1/deployments" {
			deployments++
		}
	}
	if deployments != 2 {
		t.Errorf("deployments GETs = %d, want exactly the first page and its nextLink continuation", deployments)
	}
}

// A continuation link that does not stay on the https project host —
// an off-host https link or a same-host plain-http one — is dropped, so the
// Entra bearer never leaves the project host. The truncated listing counts
// as complete: page 1 only, one deployments GET, cached for the full TTL.
func TestAzureFoundry_ProjectDeploymentsRejectOffHostNextLink(t *testing.T) {
	rejectedNextLink := func(t *testing.T, linkFor func(r *http.Request) string) {
		t.Helper()
		srv := newAzureTLSTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
			switch r.URL.Path {
			case "/api/projects/p1/deployments":
				writeJSON(w, 200, map[string]any{
					"value": []map[string]any{
						{"name": "page1-gpt4o", "modelName": "gpt-4o", "modelPublisher": "OpenAI", "capabilities": map[string]any{"chat_completion": "true"}},
					},
					"nextLink": linkFor(r),
				})
			case "/api/projects/p1/agents":
				writeJSON(w, 200, map[string]any{"data": []map[string]any{}})
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
				w.WriteHeader(404)
			}
		})
		p := newAzureAgentProvider(t, srv)
		models, err := p.Models(context.Background())
		if err != nil {
			t.Fatalf("Models: %v", err)
		}
		if !hasModel(models, "page1-gpt4o") || hasModel(models, "page2-phi4") {
			t.Errorf("listing = %+v, want page 1 only", models)
		}
		deployments := 0
		for _, r := range srv.requests() {
			if r.Path == "/api/projects/p1/deployments" {
				deployments++
			}
		}
		if deployments != 1 {
			t.Errorf("deployments GETs = %d, want the rejected continuation to end the listing after page 1", deployments)
		}
		// The listing is complete, not degraded: the second Models call is a
		// cache hit and the cache keeps the full TTL.
		n := len(srv.requests())
		if _, err := p.Models(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(srv.requests()) != n {
			t.Error("a listing whose continuation was rejected is complete and must be cached")
		}
		azureModelsCache.mu.Lock()
		entry, cached := azureModelsCache.m[azureModelsCacheKey(p.cfg)]
		azureModelsCache.mu.Unlock()
		if !cached {
			t.Fatal("the listing must be cached")
		}
		if ttl := time.Until(entry.expires); ttl <= azureModelsRetryAfter {
			t.Errorf("the truncated listing is complete, want the full cache TTL, got %v", ttl)
		}
	}

	t.Run("off_host_https_link", func(t *testing.T) {
		rejectedNextLink(t, func(_ *http.Request) string {
			return "https://evil.example/api/projects/p1/deployments?api-version=v1&skipToken=2"
		})
	})
	t.Run("same_host_http_link", func(t *testing.T) {
		rejectedNextLink(t, func(r *http.Request) string {
			return "http://" + r.Host + "/api/projects/p1/deployments?api-version=v1&skipToken=2"
		})
	})
}
