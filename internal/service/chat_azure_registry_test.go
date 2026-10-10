package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/egress"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// Chat turns on the Azure AI Foundry model registry (Part 9): the model id
// ("m/<entryId>", a pre-registry id for one release, "" for the server default)
// resolves before any stream starts, and every reader of the model takes the
// resolved provider-level id — never the request's.

// azChatEnv is a ChatService on the model registry: azmEnv's AzureModelService (fake
// registry, keystores, organisation directory) with org-1 (admin "admin", member
// "u1", viewer "viewer"), and a chat factory on the same keystores recording usage
// rows in backend; the chat service's organisation role lookup (credentialOrg) reads
// the same directory, and conversations stay in the local file store.
type azChatEnv struct {
	*azmEnv
	chat    *ChatService
	backend *azChatBackend
}

// azChatBackend is the chat store: usage rows and daily usage from
// testutil.FakeBackend, the caller's organisation roles from the registry env's directory.
type azChatBackend struct {
	*testutil.FakeBackend
	orgs *azmOrgDir
}

func (b *azChatBackend) OrgMemberRole(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	return b.orgs.OrgMemberRole(ctx, orgID, userID)
}

func newAzChatEnv(t *testing.T) *azChatEnv {
	t.Helper()
	e := &azChatEnv{azmEnv: newAzureModelsEnv(t)}
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "u1": auth.RoleMember, "viewer": auth.RoleViewer})
	e.backend = &azChatBackend{FakeBackend: testutil.NewFakeBackend(), orgs: e.orgs}
	factory := ai.NewProviderFactory(func(scope, provider string) (string, error) {
		v, _ := e.secrets.Get(scope, provider)
		return v, nil
	}, nil, e.backend.SaveUsageMetric, nil)
	factory.SetOrgKeyLookup(e.orgKeys.OrgKeyLookup)
	e.chat = &ChatService{
		notifier:      &testutil.CountingNotifier{},
		configDir:     t.TempDir(),
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
		settings:      nilSettings{},
		backend:       e.backend,
		chatCtxCache:  newChatContextCache(),
	}
	e.chat.azure = e.svc
	return e
}

// turn streams one chat turn to its end and returns its final state.
func (e *azChatEnv) turn(t *testing.T, scope string, doc *models.FlowDocument, req models.ChatRequest) resumeSnapshot {
	t.Helper()
	id, err := e.chat.StreamChatMessage(context.Background(), scope, doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage(%q): %v", req.Model, err)
	}
	e.chat.BeginStream(context.Background(), id)
	return waitStreamFinished(t, e.chat, id).snapshot()
}

// lastStored is the last message of a stored conversation.
func (e *azChatEnv) lastStored(t *testing.T, doc *models.FlowDocument, convKey string) models.ChatMessage {
	t.Helper()
	stored, err := e.chat.GetConversation(context.Background(), doc.ID, convKey)
	if err != nil || len(stored) == 0 {
		t.Fatalf("stored conversation = %+v, %v", stored, err)
	}
	return stored[len(stored)-1]
}

// waitUsage waits for n usage rows (the audited wrapper records them in the
// background) and returns them.
func (e *azChatEnv) waitUsage(t *testing.T, n int) []*storageif.UsageMetric {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := e.backend.SavedUsageMetrics()
		if len(rows) >= n {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d usage row(s) recorded, want %d", len(rows), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// scriptProviders routes the chat service's registry provider builds to p
// (the azureConnection seam) and returns the resources it was asked for.
func (e *azChatEnv) scriptProviders(p ai.Provider) func() []ai.AzureConnectionRef {
	var mu sync.Mutex
	var refs []ai.AzureConnectionRef
	e.chat.azureConnection = func(_ context.Context, _ string, ref ai.AzureConnectionRef, _ []ai.AzureRegisteredModel) (ai.Provider, error) {
		mu.Lock()
		defer mu.Unlock()
		refs = append(refs, ref)
		return p, nil
	}
	return func() []ai.AzureConnectionRef {
		mu.Lock()
		defer mu.Unlock()
		return append([]ai.AzureConnectionRef(nil), refs...)
	}
}

// pricedSpy is a scripted registry provider (itemsProvider: it records every request
// and lists only what the test gives it) priced under a non-alias id at one dollar
// per output token, so the stream's in-flight reservation reads as the tokens it
// reserved.
type pricedSpy struct{ *itemsProvider }

func (p *pricedSpy) PricePerMillionTokens() ai.Pricing {
	return ai.Pricing{OutputCostPerM: 1_000_000}
}

// azChatFoundry is a fake Foundry resource the real registry provider talks to (the
// ai.AzureFoundryOptions.Transport seam): a chat completion streams "Hello from
// Foundry." answered by gpt-4o-2024-11-20 with usage 12/3; with a status set, every
// request is answered with it instead (401: a rejected key). It records each
// request's path, api-key header and JSON body.
type azChatFoundry struct {
	mu     sync.Mutex
	status int
	reqs   []azChatFoundryRequest
}

type azChatFoundryRequest struct {
	path   string
	apiKey string
	body   map[string]any
}

func newAzChatFoundry(t *testing.T) *azChatFoundry {
	t.Helper()
	f := &azChatFoundry{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	rt := &rewriteTransport{target: strings.TrimPrefix(srv.URL, "http://"), base: &http.Transport{}}
	t.Cleanup(rt.base.CloseIdleConnections)
	ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{Egress: egress.AllowLoopback, ExtraAllowedHosts: []string{azureTestHost}, Transport: rt})
	ai.ResetAzureFoundryCaches()
	t.Cleanup(func() {
		ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{})
		ai.ResetAzureFoundryCaches()
	})
	return f
}

const azChatServed = "gpt-4o-2024-11-20"

func (f *azChatFoundry) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.reqs = append(f.reqs, azChatFoundryRequest{path: r.URL.Path, apiKey: r.Header.Get("api-key"), body: body})
	status := f.status
	f.mu.Unlock()
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"code":"401","message":"Access denied due to invalid subscription key or wrong API endpoint."}}`)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(rest string) string {
		return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"` + azChatServed + `",` + rest + "}\n\n"
	}
	_, _ = io.WriteString(w, chunk(`"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello from Foundry."},"finish_reason":null}]`)+
		chunk(`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)+
		chunk(`"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`)+
		"data: [DONE]\n\n")
}

func (f *azChatFoundry) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *azChatFoundry) requests() []azChatFoundryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]azChatFoundryRequest(nil), f.reqs...)
}

// A turn on a registry entry runs on the entry's resource with the entry's deployment
// (the wire model is the deployment, the output cap the entry's), the usage row names
// the deployment on provider azure-foundry, and the stored answer keeps the "m/" id
// the client asked with plus the entry's display name.
func TestChatAzureRegistry_TurnRunsOnTheResolvedEntry(t *testing.T) {
	e := newAzChatEnv(t)
	foundry := newAzChatFoundry(t)
	c := e.mustConn(t, azmUser, "contoso-prod", azmKeyConfig("personal-key"))
	docs := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-5-mini",
		DisplayName: "Docs writer", Purpose: "documentation", MaxOutputTokens: 3000})
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}
	req := models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: docs.ModelID, MaxTokens: 1000,
		UserMessage: "Document this flow", ConversationID: uuid.NewString()}

	snap := e.turn(t, "u1", doc, req)
	if !snap.Done || snap.Text != "Hello from Foundry." {
		t.Fatalf("turn = %+v", snap)
	}
	reqs := foundry.requests()
	if len(reqs) != 1 {
		t.Fatalf("the resource received %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.path != "/openai/v1/chat/completions" || r.apiKey != "personal-key" || r.body["model"] != "gpt-5-mini" {
		t.Errorf("request = %s, api-key %q, model %v; want the entry's deployment on its resource", r.path, r.apiKey, r.body["model"])
	}
	// The request asked for 1000; the gpt-5 reasoning floor raises it, never above
	// the entry's 3000.
	if got := r.body["max_completion_tokens"]; got != float64(3000) {
		t.Errorf("max_completion_tokens = %v, want the entry's max output 3000", got)
	}

	rows := e.waitUsage(t, 1)
	if u := rows[0]; u.Provider != ai.AzureFoundryProviderID || u.Model != "gpt-5-mini" || u.UserID != "u1" || u.OrgID != "" ||
		u.PromptTokens != 12 || u.CompletionTokens != 3 {
		t.Errorf("usage row = %+v, want the deployment id on azure-foundry", u)
	}

	last := e.lastStored(t, doc, req.ConversationKey())
	if last.Role != "assistant" || last.Content != "Hello from Foundry." || last.Model != docs.ModelID || last.ModelLabel != "Docs writer" ||
		last.Provider != ai.AzureFoundryProviderID || last.ServedModel != azChatServed {
		t.Errorf("stored answer = %+v, want model %s labelled Docs writer", last, docs.ModelID)
	}
}

// Every reader of the model takes the resolved id: the reservation, the generation
// params, the stream request's Model, the window its history is trimmed to — none of
// them the request's "m/<entryId>", which the request (passed by value) still carries.
func TestChatAzureRegistry_EveryReaderTakesTheResolvedID(t *testing.T) {
	ctx := context.Background()
	e := newAzChatEnv(t)
	c := e.mustConn(t, azmUser, "res", azmKeyConfig("k"))
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-5-mini",
		DisplayName: "Docs writer", MaxOutputTokens: 3000, ContextWindow: 20000})
	// The listing sizes the resolved id only; the provider-wide window is huge, so a
	// lookup by the wrong id would neither cap nor trim.
	spy := &pricedSpy{&itemsProvider{id: "registry-spy", limit: 1 << 20,
		models: []ai.ModelInfo{{ID: "gpt-5-mini", ContextLimit: 20000, MaxOutputTokens: 3000}}}}
	built := e.scriptProviders(spy)
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}
	req := models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: m.ModelID, MaxTokens: 1000, UserMessage: "q"}

	res, err := e.svc.ResolveChat(ctx, "u1", "", m.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	ctl := &streamCtl{ownerID: "u1"}
	ts, err := e.chat.prepareTurn(ctx, "u1", doc, req, &res, ctl)
	if err != nil {
		t.Fatalf("prepareTurn: %v", err)
	}
	want := turnModel{resolved: "gpt-5-mini", requested: m.ModelID, label: "Docs writer", kind: "chat", maxOutput: 3000}
	if ts.model != want {
		t.Errorf("turn model = %+v, want %+v", ts.model, want)
	}
	// Reserved: the gpt-5 floor capped at the entry's 3000 (the request's own id
	// would give 1000).
	if got, wantCost := ctl.inflight(), inFlightCost(spy.PricePerMillionTokens(), 0, 3000); got != wantCost {
		t.Errorf("reservation = %v, want %v (3000 tokens)", got, wantCost)
	}
	if req.Model != m.ModelID {
		t.Errorf("the request's model changed to %q", req.Model)
	}

	// A long history the entry's 20000-token window cannot hold.
	long := strings.Repeat("x", 20000) // 5000 tokens at the spy's len/4
	req.Messages = []models.ChatMessage{
		{Role: "user", Content: long}, {Role: "assistant", Content: long},
		{Role: "user", Content: long}, {Role: "assistant", Content: long},
	}
	snap := e.turn(t, "u1", doc, req)
	if !snap.Done {
		t.Fatalf("turn = %+v", snap)
	}
	sent := spy.requests()
	if len(sent) != 1 {
		t.Fatalf("the provider received %d requests, want 1", len(sent))
	}
	got := sent[0]
	if got.Model != "gpt-5-mini" {
		t.Errorf("stream request model = %q, want the deployment", got.Model)
	}
	if got.MaxTokens != 3000 {
		t.Errorf("stream request max tokens = %d, want 3000 (the floor capped at the entry's max output)", got.MaxTokens)
	}
	if len(got.Messages) >= len(req.Messages)+1 {
		t.Errorf("sent %d messages: the history was not trimmed to the entry's 20000-token window", len(got.Messages))
	}
	if len(got.Tools) != 0 {
		t.Error("a chat entry without UseTools ran the tool loop")
	}
	if refs := built(); len(refs) != 2 || refs[1] != (ai.AzureConnectionRef{ConnID: c.ID}) {
		t.Errorf("providers built for %+v, want the entry's own resource", refs)
	}
}

// An agent entry always runs through the tool loop — its "m/" id says nothing about
// the kind, the resolution does — even when the thread's tools toggle is off.
func TestChatAzureRegistry_AgentEntryAlwaysRunsTheToolLoop(t *testing.T) {
	e := newAzChatEnv(t)
	c := e.mustConn(t, azmUser, "project", azmEntraConfig("s"))
	agent := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "agent", Deployment: "reviewer", DisplayName: "Review bot"})
	spy := &itemsProvider{id: ai.AzureFoundryProviderID, models: []ai.ModelInfo{{ID: "agent:reviewer"}}}
	e.scriptProviders(spy)
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}

	snap := e.turn(t, "u1", doc, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: agent.ModelID, UserMessage: "Review my flow"})
	if !snap.Done {
		t.Fatalf("turn = %+v", snap)
	}
	sent := spy.requests()
	if len(sent) != 1 || sent[0].Model != "agent:reviewer" || len(sent[0].Tools) == 0 {
		t.Fatalf("requests = %+v, want one agent:reviewer request offering baki's tools", sent)
	}
	if last := e.lastStored(t, doc, "flow"); last.Model != agent.ModelID || last.ModelLabel != "Review bot" {
		t.Errorf("stored answer = %+v", last)
	}
}

// A model the caller cannot use is the create request's own answer — ErrModelNotAvailable
// before any stream exists or any provider is built — and the preview's.
func TestChatAzureRegistry_ModelNotAvailableBeforeAnyStream(t *testing.T) {
	ctx := context.Background()
	e := newAzChatEnv(t)
	mine := e.mustConn(t, azmUser, "mine", azmKeyConfig("k"))
	emb := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: mine.ID, Kind: "embedding", Deployment: "emb"})
	own := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: mine.ID, Kind: "chat", Deployment: "gpt-4o"})
	orgScope := AzureScope{Scope: "admin", OrgID: "org-1"}
	oc := e.mustConn(t, orgScope, "team", azmKeyConfig("org-key"))
	shared := e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "gpt-4.1"})
	built := e.scriptProviders(&itemsProvider{id: ai.AzureFoundryProviderID})
	personal := &models.FlowDocument{ID: "f1", Name: "f1"}
	orgFlow := &models.FlowDocument{ID: "f2", Name: "f2", OrganizationID: "org-1"}

	for _, c := range []struct {
		name, scope, model string
		doc                *models.FlowDocument
	}{
		{"an unknown entry", "u1", "m/zzzzzzzzzz", personal},
		{"a malformed entry id", "u1", "m/NOPE", personal},
		{"an embedding model", "u1", emb.ModelID, personal},
		{"another user's entry", "u2", own.ModelID, personal},
		{"an organisation's entry on a personal flow", "u1", shared.ModelID, personal},
		{"an organisation's entry for its viewer", "viewer", shared.ModelID, orgFlow},
		{"a raw id no resource of the caller answers", "u2", "gpt-4o", personal},
	} {
		req := models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: c.model, UserMessage: "q"}
		if id, err := e.chat.StreamChatMessage(ctx, c.scope, c.doc, nil, req); !errors.Is(err, ErrModelNotAvailable) || id != "" {
			t.Errorf("%s: create = %q, %v; want ErrModelNotAvailable", c.name, id, err)
		}
		if _, err := e.chat.PreviewContext(ctx, c.scope, c.doc, nil, req); !errors.Is(err, ErrModelNotAvailable) {
			t.Errorf("%s: preview = %v; want ErrModelNotAvailable", c.name, err)
		}
	}
	streams := 0
	e.chat.activeStreams.Range(func(_, _ any) bool { streams++; return true })
	if streams != 0 || len(built()) != 0 {
		t.Errorf("%d stream(s) started and %d provider(s) built for refused models", streams, len(built()))
	}
	// The organisation's member may use its entry on its flow.
	if snap := e.turn(t, "u1", orgFlow, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: shared.ModelID, UserMessage: "q"}); !snap.Done {
		t.Errorf("member on the org flow = %+v", snap)
	}
	if refs := built(); len(refs) != 1 || refs[0] != (ai.AzureConnectionRef{OrgID: "org-1", ConnID: oc.ID}) {
		t.Errorf("providers built for %+v, want the organisation's resource", refs)
	}
}

// A registry the chat path cannot read is never a fallback to another model or
// credential, and its raw error never reaches the client: the turn and the preview
// both fail with the generic setup error (PROVIDER_UNAVAILABLE).
func TestChatAzureRegistry_RegistryFailureIsTheGenericSetupError(t *testing.T) {
	ctx := context.Background()
	e := newAzChatEnv(t)
	c := e.mustConn(t, azmUser, "mine", azmKeyConfig("k"))
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-4o"})
	built := e.scriptProviders(&itemsProvider{id: ai.AzureFoundryProviderID})
	const secret = "pq: connection to 10.0.0.5 refused (password=hunter2)"
	e.reg.FailOn("ListModels", errors.New(secret))
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}
	req := models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: m.ModelID, UserMessage: "q"}

	snap := e.turn(t, "u1", doc, req)
	if snap.Done || snap.ErrorCode != ChatErrProviderUnavailable || snap.Error != errProviderSetup.Error() || snap.ErrorShared {
		t.Errorf("turn = %+v, want the generic setup error", snap)
	}
	if strings.Contains(snap.Error, "hunter2") || strings.Contains(snap.Error, "10.0.0.5") {
		t.Errorf("the store's error reached the client: %q", snap.Error)
	}
	if _, err := e.chat.PreviewContext(ctx, "u1", doc, nil, req); !errors.Is(err, errProviderSetup) {
		t.Errorf("preview = %v, want the generic setup error", err)
	}
	if len(built()) != 0 {
		t.Error("a provider was built although the model could not be resolved")
	}
}

// An organisation's entry runs on the organisation's credential — never the member's
// own, even for the same deployment name — bills the flow's org, and its credential
// failures are the org admin's to fix: the shared flag and the "ask your admin" copy.
func TestChatAzureRegistry_OrgEntryRunsOnTheOrgCredential(t *testing.T) {
	e := newAzChatEnv(t)
	foundry := newAzChatFoundry(t)
	orgScope := AzureScope{Scope: "admin", OrgID: "org-1"}
	oc := e.mustConn(t, orgScope, "team", azmKeyConfig("org-key"))
	shared := e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "gpt-4o", DisplayName: "Team GPT"})
	own := e.mustConn(t, azmUser, "mine", azmKeyConfig("personal-key"))
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: own.ID, Kind: "chat", Deployment: "gpt-4o"})
	doc := &models.FlowDocument{ID: "f-org", Name: "f-org", OrganizationID: "org-1"}
	req := models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: shared.ModelID, UserMessage: "Shared question", ConversationID: uuid.NewString()}

	snap := e.turn(t, "u1", doc, req)
	if !snap.Done || snap.ErrorShared {
		t.Fatalf("turn = %+v", snap)
	}
	if reqs := foundry.requests(); len(reqs) != 1 || reqs[0].apiKey != "org-key" || reqs[0].body["model"] != "gpt-4o" {
		t.Fatalf("requests = %+v, want one gpt-4o request on the organisation's key", reqs)
	}
	if u := e.waitUsage(t, 1)[0]; u.OrgID != "org-1" || u.UserID != "u1" || u.Model != "gpt-4o" {
		t.Errorf("usage row = %+v, want the member's turn billed to org-1", u)
	}
	if last := e.lastStored(t, doc, req.ConversationKey()); last.Model != shared.ModelID || last.ModelLabel != "Team GPT" {
		t.Errorf("stored answer = %+v", last)
	}

	// The resource refuses the organisation's key.
	foundry.setStatus(http.StatusUnauthorized)
	snap = e.turn(t, "u1", doc, req)
	if snap.ErrorCode != ChatErrProviderAuth || !snap.ErrorShared || !strings.Contains(snap.Error, "ask your organisation admin") {
		t.Errorf("rejected org key = %+v, want PROVIDER_AUTH with the shared flag and the admin copy", snap)
	}

	// The organisation's credential is gone (its lender was demoted): no fallback to
	// the member's own key for the same deployment.
	foundry.setStatus(0)
	if err := e.orgKeys.DeleteOrg("org-1", ai.AzureConnectionKey(oc.ID)); err != nil {
		t.Fatal(err)
	}
	before := len(foundry.requests())
	snap = e.turn(t, "u1", doc, req)
	if snap.ErrorCode != ChatErrProviderNotConfigured || !snap.ErrorShared || !strings.Contains(snap.Error, "has no credentials") ||
		!strings.Contains(snap.Error, "ask your organisation admin") {
		t.Errorf("org resource without credentials = %+v", snap)
	}
	if n := len(foundry.requests()) - before; n != 0 {
		t.Errorf("the turn reached a resource %d time(s)", n)
	}
}

// A personal resource whose credential is gone fails with copy that names
// the resource, not an API key.
func TestChatAzureRegistry_PersonalResourceWithoutCredentials(t *testing.T) {
	e := newAzChatEnv(t)
	newAzChatFoundry(t)
	c := e.mustConn(t, azmUser, "mine", azmKeyConfig("k"))
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-4o"})
	if err := e.secrets.Delete("u1", ai.AzureConnectionKey(c.ID)); err != nil {
		t.Fatal(err)
	}
	snap := e.turn(t, "u1", &models.FlowDocument{ID: "f1", Name: "f1"}, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: m.ModelID, UserMessage: "q"})
	if snap.ErrorCode != ChatErrProviderNotConfigured || snap.ErrorShared || !strings.Contains(snap.Error, "resource has no stored credentials") {
		t.Errorf("turn = %+v", snap)
	}
}

// model "" is the server default — the entry the legacy chat deployment became —
// stored with that entry's "m/" id; a raw pre-registry id still resolves through its
// alias for one release and is stored as sent.
func TestChatAzureRegistry_ServerDefaultAndLegacyIDs(t *testing.T) {
	e := newAzChatEnv(t)
	foundry := newAzChatFoundry(t)
	if err := e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"legacy-chat","deployments":["legacy-extra"]`)); err != nil {
		t.Fatal(err)
	}
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}

	conv := uuid.NewString()
	snap := e.turn(t, "u1", doc, models.ChatRequest{Provider: ai.AzureFoundryProviderID, UserMessage: "q", ConversationID: conv})
	if !snap.Done {
		t.Fatalf("default turn = %+v", snap)
	}
	reqs := foundry.requests()
	if len(reqs) != 1 || reqs[0].body["model"] != "legacy-chat" || reqs[0].apiKey != "legacy-key" {
		t.Fatalf("requests = %+v, want the legacy chat deployment", reqs)
	}
	wantID := "m/" + legacyAzureEntryID(e.svc.userOwner("u1"), "chat", "legacy-chat")
	if last := e.lastStored(t, doc, conv); last.Model != wantID || last.ModelLabel != "legacy-chat" {
		t.Errorf("stored answer = %+v, want the server default's %s", last, wantID)
	}

	conv = uuid.NewString()
	if snap := e.turn(t, "u1", doc, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: "legacy-extra", UserMessage: "q", ConversationID: conv}); !snap.Done {
		t.Fatalf("raw id turn = %+v", snap)
	}
	if reqs := foundry.requests(); len(reqs) != 2 || reqs[1].body["model"] != "legacy-extra" {
		t.Errorf("raw id request = %+v", reqs)
	}
	if last := e.lastStored(t, doc, conv); last.Model != "legacy-extra" || last.ModelLabel != "legacy-extra" {
		t.Errorf("stored raw-id answer = %+v", last)
	}
}

// The preview resolves exactly as the turn does: the window is the resolved entry's
// (its context-window override, through the audited listing), and nothing reaches Azure.
func TestChatAzureRegistry_PreviewUsesTheEntrysWindow(t *testing.T) {
	ctx := context.Background()
	e := newAzChatEnv(t)
	foundry := newAzChatFoundry(t)
	c := e.mustConn(t, azmUser, "mine", azmKeyConfig("k"))
	small := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "distilled", ContextWindow: 20000})
	big := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-4o"})
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}

	p, err := e.chat.PreviewContext(ctx, "u1", doc, nil, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: small.ModelID, UserMessage: "q"})
	if err != nil || p.ContextLimit != 20000 {
		t.Fatalf("preview = %+v, %v; want the entry's 20000-token window", p, err)
	}
	p, err = e.chat.PreviewContext(ctx, "u1", doc, nil, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: big.ModelID, UserMessage: "q"})
	if err != nil || p.ContextLimit != 128000 {
		t.Errorf("preview = %+v, %v; want gpt-4o's catalogue window", p, err)
	}
	if n := len(foundry.requests()); n != 0 {
		t.Errorf("the preview made %d request(s) to Azure", n)
	}
}

// Every other provider keeps its path: the registry is never read.
func TestChatAzureRegistry_OtherProvidersNeverReadTheRegistry(t *testing.T) {
	registerFakeProvider()
	e := newAzChatEnv(t)
	if err := e.secrets.Save("u1", "fake", "k"); err != nil {
		t.Fatal(err)
	}
	e.reg.ResetCalls()
	snap := e.turn(t, "u1", &models.FlowDocument{ID: "f1", Name: "f1"}, models.ChatRequest{Provider: "fake", Model: "fake-model", UserMessage: "q"})
	if !snap.Done {
		t.Fatalf("turn = %+v", snap)
	}
	for _, method := range []string{"ImportedAt", "ListConnections", "ListModels"} {
		if n := e.reg.Calls(method); n != 0 {
			t.Errorf("a %q turn called the registry's %s %d time(s)", "fake", method, n)
		}
	}
}

// The reasoning floor never raises max tokens above the model's output ceiling — a
// registry entry's "Max output tokens" included — and a request that left max tokens
// unset is capped by that override too.
func TestReasoningFloor_NeverAboveTheOutputCeiling(t *testing.T) {
	for _, tc := range []struct {
		model              string
		in, maxOutput, out int
	}{
		{"gpt-5-mini", 1000, 0, 16384},   // no ceiling known: the floor
		{"gpt-5-mini", 1000, 3000, 3000}, // the floor, capped
		{"gpt-5-mini", 0, 3000, 3000},    // unset: raised to the capped floor
		{"gpt-5-mini", 5000, 3000, 5000}, // above the ceiling: the caller clamps
		{"gpt-4o", 1000, 3000, 1000},     // not a reasoning model
	} {
		if got := reasoningFloorTokens(tc.in, tc.model, "", tc.maxOutput); got != tc.out {
			t.Errorf("reasoningFloorTokens(%d, %s, ceiling %d) = %d, want %d", tc.in, tc.model, tc.maxOutput, got, tc.out)
		}
	}

	p := &itemsProvider{id: ai.AzureFoundryProviderID, limit: 1 << 20, models: []ai.ModelInfo{
		{ID: "gpt-5-mini", MaxOutputTokens: 3000},
		{ID: "distilled", MaxOutputTokens: 1024},
	}}
	for _, tc := range []struct {
		model              string
		in, override, want int
	}{
		{"gpt-5-mini", 1000, 3000, 3000},
		{"gpt-5-mini", 0, 3000, 3000},
		{"gpt-5-mini", 8000, 3000, 3000},
		{"distilled", 4096, 1024, 1024},
		{"distilled", 0, 1024, 1024}, // unset: the common default, capped at the override
		{"distilled", 0, 0, 0},       // unset, no override: the provider's own default
	} {
		_, got := turnGenerationParams(context.Background(), p, models.ChatRequest{MaxTokens: tc.in}, turnModel{resolved: tc.model, maxOutput: tc.override})
		if got != tc.want {
			t.Errorf("%s with %d (override %d): max tokens %d, want %d", tc.model, tc.in, tc.override, got, tc.want)
		}
	}
}

// A client-sent modelLabel is stored clamped: NFC, no control, bidi or
// zero-width characters, whitespace collapsed, at most 64 characters.
func TestClampModelLabel(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "",
		"  Docs\twriter \n v2 ":             "Docs writer v2",
		"Docs\u202ewriter":                  "Docswriter",      // a bidi override
		"Zero\u200bwidth\u2060joined\ufeff": "Zerowidthjoined", // zero-width characters
		"NUL\x00and\x07bell":                "NULandbell",
		"Cafe\u0301":                        "Caf\u00e9", // NFC
		"bad\xffutf8":                       "badutf8",
		strings.Repeat("a", 70):             strings.Repeat("a", 64),
		strings.Repeat("\u00e9", 70):        strings.Repeat("\u00e9", 64), // characters, not bytes
		strings.Repeat("a", 63) + " b":      strings.Repeat("a", 63),
	} {
		if got := ClampModelLabel(in); got != want {
			t.Errorf("ClampModelLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
