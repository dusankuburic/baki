package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/egress"
)

// azmFakeFoundry routes the Azure runtime to an in-process fake Foundry
// (the ai.AzureFoundryOptions.Transport seam): the model catalogue route
// answers modelsStatus, chat completions answer a tiny reply. It records
// every request path.
func azmFakeFoundry(t *testing.T, modelsStatus int) *[]string {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/openai/v1/models"):
			w.WriteHeader(modelsStatus)
			_, _ = w.Write([]byte(`{"data":[],"error":{"code":"401","message":"Access denied due to invalid subscription key"}}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
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
	return &paths
}

// One test entry point: a resource test is an authenticated probe with no
// model call; a model test sends one tiny request; saved targets record
// their last test, drafts nothing; an Azure refusal is a result, not an
// error.
func TestAzureModels_TestEntryPoint(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	c := e.mustConn(t, azmUser, "c", azmKeyConfig("k"))
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-4o"})
	paths := azmFakeFoundry(t, http.StatusOK)
	e.svc.probeResource = ai.ProbeAzureFoundryResource
	e.svc.probeModel = ai.ProbeAzureFoundryModel

	res, err := e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: c.ID})
	if err != nil || !res.OK || res.Code != "" {
		t.Fatalf("resource test = %+v, %v", res, err)
	}
	if strings.Join(*paths, ",") != "GET /openai/v1/models" {
		t.Errorf("a resource test made %v, want the one authenticated probe", *paths)
	}
	*paths = nil
	res, err = e.svc.Test(ctx, azmUser, AzureTestInput{Model: &AzureTestModelRef{ID: m.ID}})
	if err != nil || !res.OK || len(*paths) != 1 || !strings.HasSuffix((*paths)[0], "/chat/completions") {
		t.Fatalf("model test = %+v, %v, %v", res, err, *paths)
	}
	if got, _ := e.svc.GetModel(ctx, azmUser, m.ID); got.LastTest == nil || !got.LastTest.OK {
		t.Errorf("the saved model's last test = %+v", got.LastTest)
	}
	res, err = e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: c.ID, Model: &AzureTestModelRef{Kind: "chat", Deployment: "draft"}})
	if err != nil || !res.OK {
		t.Fatalf("draft model test = %+v, %v", res, err)
	}
	// An agent on an API-key resource is refused before any request.
	*paths = nil
	res, err = e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: c.ID, Model: &AzureTestModelRef{Kind: "agent", Deployment: "helper"}})
	if err != nil || res.OK || len(*paths) != 0 {
		t.Errorf("agent on an API key = %+v, %v, %v", res, err, *paths)
	}
	// Invalid requests are errors.
	if _, err := e.svc.Test(ctx, azmUser, AzureTestInput{Model: &AzureTestModelRef{ID: "zzzzzzzzzz"}}); azmCode(err) != AzureCodeModelNotFound {
		t.Errorf("unknown model: %v", err)
	}
	if _, err := e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: "zzzzzzzzzz"}); azmCode(err) != AzureCodeConnectionNotFound {
		t.Errorf("unknown resource: %v", err)
	}
	if _, err := e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: c.ID, Model: &AzureTestModelRef{Kind: "vision", Deployment: "x"}}); azmCode(err) != ai.AzureCfgErrInvalidKind {
		t.Errorf("draft of an unknown kind: %v", err)
	}
	if _, err := e.svc.Test(ctx, azmUser, AzureTestInput{}); azmCode(err) != ai.AzureCfgErrInvalidConfig {
		t.Errorf("nothing to test: %v", err)
	}

	// A rejected key is a failed result with its code, recorded.
	azmFakeFoundry(t, http.StatusUnauthorized)
	res, err = e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: c.ID})
	if err != nil || res.OK || res.Code != "INVALID_API_KEY" || res.Error == "" {
		t.Fatalf("rejected key = %+v, %v", res, err)
	}
	if v, _ := e.svc.GetConnection(ctx, azmUser, c.ID); v.LastTest == nil || v.LastTest.OK || v.LastTest.Error == "" {
		t.Errorf("failed test not recorded: %+v", v.LastTest)
	}
	// A resource without credentials: a result, no request.
	_ = e.secrets.Delete("u1", "azure-foundry/"+c.ID)
	if res, err := e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: c.ID}); err != nil || res.OK || res.Code != "NEEDS_CREDENTIALS" {
		t.Errorf("no credentials = %+v, %v", res, err)
	}
}

// Discovery: the project's deployments and agents, marked when already
// added; an Azure refusal is a 200 with error and code; a resource that
// cannot list its project is DISCOVERY_UNAVAILABLE.
func TestAzureModels_Discover(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	en := e.mustConn(t, azmUser, "entra", azmEntraConfig("s"))
	added := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: en.ID, Kind: "chat", Deployment: "gpt-4o"})
	var registered []ai.AzureRegisteredModel
	e.svc.discover = func(_ context.Context, _ string, reg []ai.AzureRegisteredModel) (ai.AzureDiscovery, error) {
		registered = reg
		return ai.AzureDiscovery{
			Deployments: []ai.AzureDiscoveredDeployment{{Name: "gpt-4o", Kind: "chat", Model: "gpt-4o", AlreadyAdded: true, ModelID: added.ModelID}, {Name: "emb", Kind: "embedding"}},
			Agents:      []ai.AzureDiscoveredAgent{{Name: "helper", Kind: "agent", Deployment: "gpt-4o", Workflow: true}},
			Warnings:    []string{"1 agent skipped"},
		}, nil
	}
	d, err := e.svc.Discover(ctx, azmUser, en.ID)
	if err != nil || len(d.Deployments) != 2 || len(d.Agents) != 1 || len(d.Warnings) != 1 || d.Error != "" {
		t.Fatalf("discovery = %+v, %v", d, err)
	}
	if len(registered) != 1 || registered[0].EntryID != added.ID {
		t.Errorf("registered entries passed = %+v", registered)
	}
	if !d.Deployments[0].AlreadyAdded || d.Deployments[0].ModelID != added.ModelID || !d.Agents[0].Workflow {
		t.Errorf("items = %+v", d)
	}
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), `"model":""`) {
		t.Errorf("an unknown model must be sent as an empty string: %s", b)
	}
	e.svc.discover = func(context.Context, string, []ai.AzureRegisteredModel) (ai.AzureDiscovery, error) {
		return ai.AzureDiscovery{}, &ai.AzureProjectRefusalError{Status: 403, Message: "the principal has no role on the project"}
	}
	d, err = e.svc.Discover(ctx, azmUser, en.ID)
	if err != nil || d.Error == "" || d.Code != "PERMISSION_DENIED" || d.Deployments == nil {
		t.Errorf("refusal = %+v, %v", d, err)
	}
	e.svc.discover = func(context.Context, string, []ai.AzureRegisteredModel) (ai.AzureDiscovery, error) {
		return ai.AzureDiscovery{}, fmt.Errorf("%w: an API key cannot list a project", ai.ErrAzureDiscoveryUnavailable)
	}
	if _, err := e.svc.Discover(ctx, azmUser, en.ID); azmCode(err) != AzureCodeDiscoveryUnavailable || strings.HasPrefix(err.Error(), "azure ai foundry") {
		t.Errorf("unavailable: %v", err)
	}
	if _, err := e.svc.Discover(ctx, azmUser, "zzzzzzzzzz"); azmCode(err) != AzureCodeConnectionNotFound {
		t.Errorf("unknown resource: %v", err)
	}
}
