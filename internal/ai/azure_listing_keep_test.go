package ai

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// What a cut-short project listing stores (R-provider-c2) and the budget a listing runs under (R-provider-c3):
// an incomplete refresh never replaces the complete listing a registry provider keeps serving through the grace,
// it only pushes back the next attempt; a deployments listing that answered keeps the previous agents; pages read
// before a deadline are kept; the next attempt honours the project's Retry-After; and a refresher's deadline, not
// the fallback cap, bounds the detached flight.

// listingProject is a project whose two listings answer as the test last set them.
type listingProject struct {
	mu          sync.Mutex
	deployments http.HandlerFunc
	agents      http.HandlerFunc
	calls       atomic.Int32
}

func newListingProject(t *testing.T, tls bool) (*listingProject, *azureTestServer) {
	t.Helper()
	ResetAzureFoundryCaches()
	t.Cleanup(ResetAzureFoundryCaches)
	lp := &listingProject{}
	handler := func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		lp.calls.Add(1)
		lp.mu.Lock()
		deployments, agents := lp.deployments, lp.agents
		lp.mu.Unlock()
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			deployments(w, r)
		case "/api/projects/p1/agents":
			agents(w, r)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	if tls {
		return lp, newAzureTLSTestServer(t, handler)
	}
	return lp, newAzureTestServer(t, handler)
}

func (lp *listingProject) set(deployments, agents http.HandlerFunc) {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	lp.deployments, lp.agents = deployments, agents
}

func deploymentServing(name, model string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"value": []map[string]any{{"name": name, "modelName": model}}})
	}
}

func helperAgentsAnswer(w http.ResponseWriter, _ *http.Request) { helperAgents(w) }

func projectBusy(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 503, map[string]any{"error": map[string]any{"message": "busy"}})
}

// hangs answers nothing until the caller gives up (bounded, so a server Close never waits long).
func hangs(_ http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(3 * time.Second):
	}
}

// shortenProjectFallback makes the fallback cap of a flight without a deadline short, so a test that proves a
// listing outlives it stays fast — and fails fast against a build that bounds every flight by it.
func shortenProjectFallback(t *testing.T, d time.Duration) {
	t.Helper()
	prev := azureProjectExtrasTimeout
	azureProjectExtrasTimeout = d
	t.Cleanup(func() { azureProjectExtrasTimeout = prev })
}

func within(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func registryChatAndAgent() []AzureRegisteredModel {
	return []AzureRegisteredModel{entry("aaaaaaaaa1", AzureModelKindChat, "prod-chat"), entry("aaaaaaaaa2", AzureModelKindAgent, "Helper")}
}

func listedModel(models []ModelInfo, id string) (ModelInfo, bool) {
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return ModelInfo{}, false
}

func cachedEntry(t *testing.T, cfg AzureFoundryConfig) azureModelsEntry {
	t.Helper()
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	e, ok := azureModelsCache.m[azureModelsCacheKey(cfg)]
	if !ok {
		t.Fatal("no cached listing")
	}
	return e
}

// shiftCachedEntry moves the cached entry's clock back by d, as if d had passed.
func shiftCachedEntry(t *testing.T, cfg AzureFoundryConfig, d time.Duration) {
	t.Helper()
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	key := azureModelsCacheKey(cfg)
	e, ok := azureModelsCache.m[key]
	if !ok {
		t.Fatal("no cached listing to shift")
	}
	e.expires, e.fetchedAt = e.expires.Add(-d), e.fetchedAt.Add(-d)
	if !e.retryAt.IsZero() {
		e.retryAt = e.retryAt.Add(-d)
	}
	azureModelsCache.m[key] = e
}

func mustRefresh(t *testing.T, p *AzureFoundryProvider, ctx context.Context) {
	t.Helper()
	if err := p.refreshListing(ctx); err != nil {
		t.Fatalf("refreshListing: %v", err)
	}
}

// R-provider-c2: with a complete listing cached and expired (within the grace), a refresh whose deployments call
// times out leaves the registry provider serving the earlier listing; the failed refresh is retried no sooner than
// azureModelsRetryAfter, and a later complete refresh replaces the entry with the full TTL.
func TestAzureRegistryListing_CutShortRefreshKeepsTheCompleteListing(t *testing.T) {
	shortenProjectFallback(t, 200*time.Millisecond)
	lp, srv := newListingProject(t, false)
	lp.set(deploymentServing("prod-chat", "gpt-4.1"), helperAgentsAnswer)
	p := registryTestProvider(t, srv, registryEntraConfig(srv), registryChatAndAgent())
	mustRefresh(t, p, within(t, 5*time.Second))
	if l := p.ModelsListing(); !l.Complete || !l.Live || l.Due || time.Since(l.AsOf) > 5*time.Second {
		t.Fatalf("fresh listing state = %+v, want complete, live, not due, as of now", l)
	}
	expireAzureListing(t, p.cfg)
	if l := p.ModelsListing(); !l.Complete || l.Live || !l.Due {
		t.Fatalf("expired listing state = %+v, want complete, stale and due", l)
	}

	lp.set(hangs, helperAgentsAnswer)
	mustRefresh(t, p, within(t, 500*time.Millisecond))
	models, _ := p.Models(context.Background())
	if m, ok := listedModel(models, "prod-chat"); !ok || m.ServedModel != "gpt-4.1" {
		t.Errorf("prod-chat after a timed-out refresh = %+v (listed %v), want the earlier listing's served model gpt-4.1", m, ok)
	}
	if !hasModel(models, "agent:Helper") {
		t.Errorf("listing after a timed-out refresh = %s, want the earlier listing's agents", modelIDs(models))
	}
	e := cachedEntry(t, p.cfg)
	if !e.complete {
		t.Error("the kept listing must still read as complete")
	}
	if wait := time.Until(e.retryAt); wait <= azureModelsRetryAfter-5*time.Second || wait > azureModelsRetryAfter {
		t.Errorf("next attempt in %v, want about azureModelsRetryAfter (%v)", wait, azureModelsRetryAfter)
	}
	if l := p.ModelsListing(); !l.Complete || l.Due {
		t.Errorf("listing state after the failed refresh = %+v, want complete and not due", l)
	}

	// Not retried before the next attempt is due.
	before := lp.calls.Load()
	mustRefresh(t, p, within(t, 5*time.Second))
	if n := lp.calls.Load() - before; n != 0 {
		t.Errorf("a refresh before the retry was due made %d project requests", n)
	}

	// Due: a complete refresh replaces the entry with the full TTL.
	shiftCachedEntry(t, p.cfg, azureModelsRetryAfter+time.Second)
	lp.set(deploymentServing("prod-chat", "gpt-4o"), helperAgentsAnswer)
	mustRefresh(t, p, within(t, 5*time.Second))
	models, _ = p.Models(context.Background())
	if m, _ := listedModel(models, "prod-chat"); m.ServedModel != "gpt-4o" {
		t.Errorf("prod-chat after the complete refresh = %+v, want the new served model", m)
	}
	e = cachedEntry(t, p.cfg)
	if !e.complete || time.Until(e.expires) <= azureModelsCacheTTL-5*time.Second {
		t.Errorf("complete refresh stored complete=%v for %v, want the full TTL", e.complete, time.Until(e.expires))
	}
	if l := p.ModelsListing(); !l.Complete || !l.Live || time.Since(l.AsOf) > 5*time.Second {
		t.Errorf("listing state after the complete refresh = %+v", l)
	}
}

// R-provider-c2: a refresh whose agents call fails but whose deployments succeed keeps the previous agents and
// stores the new deployment decorations; the result reads as incomplete.
func TestAzureRegistryListing_AgentsFailureKeepsThePreviousAgents(t *testing.T) {
	lp, srv := newListingProject(t, false)
	lp.set(deploymentServing("prod-chat", "gpt-4.1"), helperAgentsAnswer)
	p := registryTestProvider(t, srv, registryEntraConfig(srv), registryChatAndAgent())
	mustRefresh(t, p, within(t, 5*time.Second))
	asOf := p.ModelsListing().AsOf
	expireAzureListing(t, p.cfg)

	lp.set(deploymentServing("prod-chat", "gpt-5-mini"), projectBusy)
	mustRefresh(t, p, within(t, 5*time.Second))
	models, _ := p.Models(context.Background())
	if m, _ := listedModel(models, "prod-chat"); m.ServedModel != "gpt-5-mini" {
		t.Errorf("prod-chat = %+v, want the new deployments listing's served model", m)
	}
	if !hasModel(models, "agent:Helper") {
		t.Errorf("listing = %s, want the previous listing's agents", modelIDs(models))
	}
	l := p.ModelsListing()
	if l.Complete {
		t.Error("a listing whose agents call failed is not complete")
	}
	if !l.AsOf.Equal(asOf) {
		t.Errorf("AsOf = %v, want the kept agents' fetch time %v", l.AsOf, asOf)
	}
}

// R-provider-c3: a refresher's deadline, not the fallback cap of a detached Models flight, bounds the listing: four
// slow deployments pages that together outlast the fallback still complete.
func TestAzureRegistryListing_SlowPagesCompleteWithinTheCallersDeadline(t *testing.T) {
	const pageDelay = 150 * time.Millisecond
	shortenProjectFallback(t, 2*pageDelay) // the four pages need twice the fallback
	lp, srv := newListingProject(t, true)
	lp.set(pagedDeployments(srv, 4, pageDelay, 0), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []map[string]any{}})
	})
	p := registryTestProvider(t, srv, registryEntraConfig(srv), registryChatAndAgent())
	mustRefresh(t, p, within(t, 45*time.Second))
	models, _ := p.Models(context.Background())
	for i := 1; i <= 4; i++ {
		if !hasModel(models, "page"+strconv.Itoa(i)) {
			t.Errorf("listing = %s, want page %d's deployment", modelIDs(models), i)
		}
	}
	if !p.ModelsListing().Complete {
		t.Error("the slow listing must complete")
	}
}

// R-provider-c3: a deployments listing that times out on page 3 of 4 keeps pages 1-2, marked incomplete.
func TestAzureRegistryListing_TimeoutKeepsThePagesRead(t *testing.T) {
	shortenProjectFallback(t, 250*time.Millisecond)
	lp, srv := newListingProject(t, true)
	lp.set(pagedDeployments(srv, 4, 0, 3), helperAgentsAnswer)
	p := registryTestProvider(t, srv, registryEntraConfig(srv), registryChatAndAgent())
	mustRefresh(t, p, within(t, 600*time.Millisecond))
	models, _ := p.Models(context.Background())
	if !hasModel(models, "page1") || !hasModel(models, "page2") || hasModel(models, "page4") {
		t.Errorf("listing = %s, want pages 1-2 only", modelIDs(models))
	}
	if m, _ := listedModel(models, "page1"); m.ServedModel != "gpt-4o" {
		t.Errorf("page1 = %+v, want its served model", m)
	}
	if p.ModelsListing().Complete || cachedEntry(t, p.cfg).complete {
		t.Error("a listing cut by its deadline is incomplete")
	}
}

// R-provider-c3: a 429 with retry-after-ms 120000 on the deployments route sets the next attempt no earlier than
// 120 s — on a cold cache and over a kept complete listing — and refreshListing asks the project nothing before.
func TestAzureRegistryListing_ThrottleRetryAfterIsHonoured(t *testing.T) {
	throttled := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("retry-after-ms", "120000")
		writeJSON(w, 429, map[string]any{"error": map[string]any{"code": "429", "message": "throttled"}})
	}
	t.Run("cold", func(t *testing.T) {
		lp, srv := newListingProject(t, false)
		lp.set(throttled, helperAgentsAnswer)
		p := registryTestProvider(t, srv, registryEntraConfig(srv), registryChatAndAgent())
		mustRefresh(t, p, within(t, 5*time.Second))
		if l := p.ModelsListing(); l.Complete || l.Due {
			t.Errorf("throttled listing state = %+v, want incomplete and not due", l)
		}
		// 60 s on: past azureModelsRetryAfter, still inside the project's 120 s.
		shiftCachedEntry(t, p.cfg, 60*time.Second)
		before := lp.calls.Load()
		mustRefresh(t, p, within(t, 5*time.Second))
		if n := lp.calls.Load() - before; n != 0 {
			t.Errorf("a refresh 60 s into a 120 s Retry-After made %d project requests", n)
		}
		shiftCachedEntry(t, p.cfg, 61*time.Second)
		mustRefresh(t, p, within(t, 5*time.Second))
		if lp.calls.Load() == before {
			t.Error("past the Retry-After the listing must be asked again")
		}
	})
	t.Run("over_a_complete_listing", func(t *testing.T) {
		lp, srv := newListingProject(t, false)
		lp.set(deploymentServing("prod-chat", "gpt-4.1"), helperAgentsAnswer)
		p := registryTestProvider(t, srv, registryEntraConfig(srv), registryChatAndAgent())
		mustRefresh(t, p, within(t, 5*time.Second))
		expireAzureListing(t, p.cfg)
		lp.set(throttled, helperAgentsAnswer)
		mustRefresh(t, p, within(t, 5*time.Second))
		if wait := time.Until(cachedEntry(t, p.cfg).retryAt); wait < 115*time.Second {
			t.Errorf("next attempt in %v, want no earlier than the project's 120 s", wait)
		}
		models, _ := p.Models(context.Background())
		if m, _ := listedModel(models, "prod-chat"); m.ServedModel != "gpt-4.1" {
			t.Errorf("prod-chat = %+v, want the kept listing", m)
		}
	})
}

// pagedDeployments serves a deployments listing of n pages, each with deployment "page<k>" (model gpt-4o) and a
// nextLink to the next; each page answers after delay, and page hangAt (0: none) hangs instead.
func pagedDeployments(srv *azureTestServer, n int, delay time.Duration, hangAt int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if s := r.URL.Query().Get("skipToken"); s != "" {
			page, _ = strconv.Atoi(s)
		}
		if page == hangAt {
			hangs(w, r)
			return
		}
		if delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		body := map[string]any{"value": []map[string]any{{"name": "page" + strconv.Itoa(page), "modelName": "gpt-4o"}}}
		if page < n {
			body["nextLink"] = srv.URL + "/api/projects/p1/deployments?api-version=v1&skipToken=" + strconv.Itoa(page+1)
		}
		writeJSON(w, 200, body)
	}
}

// The project's Retry-After is bounded: a throttled attempt — cold, or over a kept complete listing — waits at most
// ten minutes, however long the project asked for (Retry-After is any number of seconds, or a date).
func TestAzureRegistryListing_RetryAfterIsCapped(t *testing.T) {
	const most = 10 * time.Minute
	throttled := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "172800") // 48 h
		writeJSON(w, 429, map[string]any{"error": map[string]any{"code": "429", "message": "throttled"}})
	}
	t.Run("cold", func(t *testing.T) {
		lp, srv := newListingProject(t, false)
		lp.set(throttled, helperAgentsAnswer)
		p := registryTestProvider(t, srv, registryEntraConfig(srv), registryChatAndAgent())
		mustRefresh(t, p, within(t, 5*time.Second))
		if wait := time.Until(cachedEntry(t, p.cfg).nextAttempt()); wait > most || wait < azureModelsRetryAfter-5*time.Second {
			t.Errorf("next attempt in %v, want between azureModelsRetryAfter and %v", wait, most)
		}
	})
	t.Run("over_a_complete_listing", func(t *testing.T) {
		lp, srv := newListingProject(t, false)
		lp.set(deploymentServing("prod-chat", "gpt-4.1"), helperAgentsAnswer)
		p := registryTestProvider(t, srv, registryEntraConfig(srv), registryChatAndAgent())
		mustRefresh(t, p, within(t, 5*time.Second))
		shiftCachedEntry(t, p.cfg, 6*time.Minute) // expired, within the grace
		lp.set(throttled, helperAgentsAnswer)
		mustRefresh(t, p, within(t, 5*time.Second))
		if wait := time.Until(cachedEntry(t, p.cfg).nextAttempt()); wait > most {
			t.Errorf("next attempt in %v, want at most %v", wait, most)
		}
	})
}

// A kept listing is served no longer than azureModelsStaleGrace past its TTL whatever retry is pending: past it the
// registry provider lists the bare entries and the listing state reads neither complete nor live.
func TestAzureRegistryListing_KeptListingEndsWithTheGrace(t *testing.T) {
	lp, srv := newListingProject(t, false)
	lp.set(deploymentServing("prod-chat", "gpt-4.1"), helperAgentsAnswer)
	p := registryTestProvider(t, srv, registryEntraConfig(srv), registryChatAndAgent())
	mustRefresh(t, p, within(t, 5*time.Second))
	shiftCachedEntry(t, p.cfg, azureModelsCacheTTL+azureModelsStaleGrace+time.Minute)
	key := azureModelsCacheKey(p.cfg)
	azureModelsCache.mu.Lock()
	e := azureModelsCache.m[key]
	e.retryAt = time.Now().Add(48 * time.Hour) // a retry pending far past the grace (an old build's uncapped wait)
	azureModelsCache.m[key] = e
	azureModelsCache.mu.Unlock()

	if l := p.ModelsListing(); l.Complete || l.Live || !l.AsOf.IsZero() {
		t.Errorf("listing state past the grace = %+v, want nothing served", l)
	}
	if models, _ := p.Models(context.Background()); hasModel(models, "agent:Helper") {
		t.Errorf("registry listing past the grace = %s, want the entries only", modelIDs(models))
	}
	if _, ok := cachedAzureModelsUntilDue(key); ok {
		t.Error("a document provider must not be served a listing past the grace either")
	}
}
