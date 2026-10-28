package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/egress"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// R-replicas-c4: a chat turn's resolution triggers the background refresh of its resource's project listing when
// that listing is not live — once per resource while the job runs, never waiting on the project — so a cold or idle
// replica sizes the next turns from the project listing without a picker read.

// countingRefresher installs a refresher whose refresh counts its calls and runs refresh (nil: returns at once).
func countingRefresher(e *azmEnv, refresh func(ctx context.Context, raw string, entries []ai.AzureRegisteredModel) error) *atomic.Int32 {
	var calls atomic.Int32
	e.svc.refresher = newAzureListingRefresher(func(ctx context.Context, raw string, entries []ai.AzureRegisteredModel) error {
		calls.Add(1)
		if refresh == nil {
			return nil
		}
		return refresh(ctx, raw, entries)
	})
	return &calls
}

func waitCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got != want {
		t.Fatalf("refresh jobs = %d, want %d", got, want)
	}
}

// One ResolveChat of an Entra ID + project resource with no live listing triggers exactly one background refresh
// job for it; a second ResolveChat while that job runs triggers none; an API-key resource triggers nothing.
func TestAzureModels_ResolveChatTriggersOneListingRefresh(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	project := e.mustConn(t, azmUser, "project", azmEntraConfig("s"))
	chat := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: project.ID, Kind: "chat", Deployment: "prod-chat"})
	keyed := e.mustConn(t, azmUser, "keyed", azmKeyConfig("k"))
	keyedChat := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: keyed.ID, Kind: "chat", Deployment: "gpt-4.1"})
	release := make(chan struct{})
	var seen sync.Map
	calls := countingRefresher(e, func(_ context.Context, raw string, entries []ai.AzureRegisteredModel) error {
		seen.Store(raw, len(entries))
		<-release
		return nil
	})

	res, err := e.svc.ResolveChat(ctx, "u1", "", chat.ModelID)
	if err != nil || res.ResolvedModel != "prod-chat" {
		t.Fatalf("ResolveChat = %+v, %v", res, err)
	}
	waitCalls(t, calls, 1)
	if _, err := e.svc.ResolveChat(ctx, "u1", "", chat.ModelID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResolveChat(ctx, "u1", "", keyedChat.ModelID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // a second job would have started by now
	close(release)
	e.svc.refresher.wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Errorf("refresh jobs = %d, want exactly one for the project resource", got)
	}
	seen.Range(func(raw, _ any) bool {
		if !strings.Contains(raw.(string), `"mode":"entra"`) {
			t.Errorf("refreshed a resource that lists no project: %s", raw)
		}
		return true
	})
}

// ResolveChat never waits on the project: with the project endpoint hung, the turn's resolution returns at once
// while the triggered refresh is still waiting on it.
func TestAzureModels_ResolveChatDoesNotWaitOnAHungProject(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	hung := make(chan struct{})
	var projectHits atomic.Int32
	newRefreshFoundry(t, func(w http.ResponseWriter, r *http.Request) {
		projectHits.Add(1)
		select {
		case <-r.Context().Done():
		case <-hung:
		}
	})
	t.Cleanup(func() { close(hung) })
	project := e.mustConn(t, azmUser, "project", azmEntraConfig("s"))
	chat := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: project.ID, Kind: "chat", Deployment: "prod-chat"})
	calls := countingRefresher(e, func(ctx context.Context, raw string, entries []ai.AzureRegisteredModel) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return ai.RefreshAzureFoundryListing(ctx, raw, entries)
	})

	start := time.Now()
	if _, err := e.svc.ResolveChat(ctx, "u1", "", chat.ModelID); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("ResolveChat took %v with the project endpoint hung", elapsed)
	}
	waitCalls(t, calls, 1)
	deadline := time.Now().Add(3 * time.Second)
	for projectHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if projectHits.Load() == 0 {
		t.Fatal("the triggered refresh never reached the project")
	}
	start = time.Now()
	if _, err := e.svc.ResolveChat(ctx, "u1", "", chat.ModelID); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("ResolveChat during the hung refresh took %v", elapsed)
	}
	e.svc.refresher.wg.Wait()
}

// After the refresh a turn's resolution triggered completes, the next turn's generation params see the project
// listing's served model and catalogue window for the deployment — no picker read in between.
func TestAzureModels_ResolveChatRefreshSizesTheNextTurn(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	newRefreshFoundry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			_, _ = io.WriteString(w, `{"value":[{"name":"prod-chat","modelName":"gpt-5-mini","modelPublisher":"OpenAI"}]}`)
		case "/api/projects/p1/agents":
			_, _ = io.WriteString(w, `{"data":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	project := e.mustConn(t, azmUser, "project", azmEntraConfig("s"))
	chat := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: project.ID, Kind: "chat", Deployment: "prod-chat"})
	calls := countingRefresher(e, ai.RefreshAzureFoundryListing)

	params := func() (int, int) {
		t.Helper()
		res, err := e.svc.ResolveChat(ctx, "u1", "", chat.ModelID)
		if err != nil {
			t.Fatal(err)
		}
		p, err := e.factory.ForAzureConnection(ctx, "u1", res.ConnectionRef, res.Entries)
		if err != nil {
			t.Fatal(err)
		}
		_, maxTokens := turnGenerationParams(ctx, p, models.ChatRequest{MaxTokens: 1000}, turnModel{resolved: res.ResolvedModel})
		return maxTokens, ai.ContextLimitFor(ctx, p, res.ResolvedModel)
	}

	cold, coldWindow := params()
	if cold != 1000 || coldWindow != azureDefaultWindow {
		t.Fatalf("cold turn: max tokens %d, window %d; want the request's 1000 and the provider-wide window (nothing sizes prod-chat yet)", cold, coldWindow)
	}
	e.svc.refresher.wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh jobs = %d, want one", calls.Load())
	}
	warm, warmWindow := params()
	want := azureCatalogueWindows(ctx)["gpt-5-mini"]
	if want == 0 {
		t.Fatal("gpt-5-mini is not catalogued")
	}
	if warmWindow != want {
		t.Errorf("next turn's window = %d, want gpt-5-mini's catalogue window %d", warmWindow, want)
	}
	if floor := ai.ReasoningOutputFloor("prod-chat", "gpt-5-mini"); floor <= 1000 || warm != floor {
		t.Errorf("next turn's max tokens = %d, want the reasoning floor %d of its served model", warm, floor)
	}
	e.svc.refresher.wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("refresh jobs = %d after the listing went live, want no further job", calls.Load())
	}
}

// newRefreshFoundry installs the Azure transport seam over a fake that signs in an Entra ID service principal
// (login.microsoftonline.com) and answers every other host with project.
func newRefreshFoundry(t *testing.T, project http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "login.microsoftonline.com" {
			serveFakeEntraID(w, r)
			return
		}
		project(w, r)
	}))
	t.Cleanup(srv.Close)
	rt := &rewriteTransport{target: strings.TrimPrefix(srv.URL, "http://"), base: &http.Transport{}}
	t.Cleanup(rt.base.CloseIdleConnections)
	ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{Egress: egress.AllowLoopback, ExtraAllowedHosts: []string{azureTestHost}, Transport: rt})
	ai.ResetAzureFoundryCaches()
	t.Cleanup(func() {
		ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{})
		ai.ResetAzureFoundryCaches()
	})
}

// serveFakeEntraID answers the client-credentials sign-in azidentity performs: instance discovery, the tenant's
// OpenID configuration, then the token endpoint.
func serveFakeEntraID(w http.ResponseWriter, r *http.Request) {
	const host = "login.microsoftonline.com"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case strings.HasSuffix(r.URL.Path, "/discovery/instance"):
		tenant := "common"
		if ae := r.URL.Query().Get("authorization_endpoint"); ae != "" {
			if u, err := url.Parse(ae); err == nil {
				tenant = strings.Split(strings.Trim(u.Path, "/"), "/")[0]
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tenant_discovery_endpoint": "https://" + host + "/" + tenant + "/v2.0/.well-known/openid-configuration",
			"api-version":               "1.1",
			"metadata":                  []map[string]any{{"preferred_network": host, "preferred_cache": "login.windows.net", "aliases": []string{host, "login.windows.net"}}},
		})
	case strings.HasSuffix(r.URL.Path, "/v2.0/.well-known/openid-configuration") && len(parts) > 0:
		base := "https://" + host + "/" + parts[0]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_endpoint":                        base + "/oauth2/v2.0/token",
			"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
			"issuer":                                base + "/v2.0",
			"jwks_uri":                              base + "/discovery/v2.0/keys",
			"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
			"response_types_supported":              []string{"code", "id_token"},
			"tenant_region_scope":                   "EU",
			"cloud_instance_name":                   "microsoftonline.com",
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": 3599, "ext_expires_in": 3599, "access_token": "entra-token"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// countingKeys counts credential reads, context-free (Get) and under the request's context (GetCtx).
type countingKeys struct {
	KeyStore
	gets, ctxGets atomic.Int32
}

func (c *countingKeys) Get(scope, provider string) (string, error) {
	c.gets.Add(1)
	return c.KeyStore.Get(scope, provider)
}

func (c *countingKeys) GetCtx(ctx context.Context, scope, provider string) (string, error) {
	c.ctxGets.Add(1)
	return c.KeyStore.Get(scope, provider)
}

func (c *countingKeys) HasCtx(_ context.Context, scope, provider string) (bool, error) {
	return c.Has(scope, provider)
}

func (c *countingKeys) KeyStates(context.Context, string, []string) (map[string]storageif.SecretState, error) {
	return nil, nil
}

func (c *countingKeys) reads() int32 { return c.gets.Load() + c.ctxGets.Load() }

// A chat turn whose resource's listing is live — or that lists no project — reads no credential to decide that
// no refresh is due; a read that is needed goes through the request's context.
func TestAzureModels_ResolveChatReadsNoCredentialWhileTheListingIsLive(t *testing.T) {
	ctx := context.Background()
	var keys *countingKeys
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) {
		d.KeyFamilies, _ = d.Secrets.(storageif.SecretFamilyLister)
		keys = &countingKeys{KeyStore: d.Secrets}
		d.Secrets, d.KeyReads = keys, keys
	})
	newRefreshFoundry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			_, _ = io.WriteString(w, `{"value":[{"name":"prod-chat","modelName":"gpt-4.1"}]}`)
		case "/api/projects/p1/agents":
			_, _ = io.WriteString(w, `{"data":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	project := e.mustConn(t, azmUser, "project", azmEntraConfig("s"))
	chat := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: project.ID, Kind: "chat", Deployment: "prod-chat"})
	keyed := e.mustConn(t, azmUser, "keyed", azmKeyConfig("k"))
	keyedChat := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: keyed.ID, Kind: "chat", Deployment: "gpt-4.1"})
	calls := countingRefresher(e, ai.RefreshAzureFoundryListing)

	before := keys.ctxGets.Load()
	for _, id := range []string{chat.ModelID, keyedChat.ModelID} {
		if _, err := e.svc.ResolveChat(ctx, "u1", "", id); err != nil {
			t.Fatal(err)
		}
	}
	if keys.ctxGets.Load() == before {
		t.Error("the cold turns' credential reads must run under the request's context")
	}
	e.svc.refresher.wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh jobs = %d, want one", calls.Load())
	}

	before = keys.reads()
	for range 3 {
		for _, id := range []string{chat.ModelID, keyedChat.ModelID} {
			if _, err := e.svc.ResolveChat(ctx, "u1", "", id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := keys.reads() - before; n != 0 {
		t.Errorf("turns on a live listing and on an API-key resource read %d credentials, want none", n)
	}
	e.svc.refresher.wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("refresh jobs = %d, want no further job", calls.Load())
	}
}
