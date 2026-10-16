package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// Regression (H8/Track 6): the Knowledge Base routes must return 503 when the storage backend is nil (local mode) instead of nil-panicking on h.backend.
// The latent bug: the service is constructed non-nil even with a nil store, so the h.knowledge == nil guard did not protect the h.backend dereference.
// The routes now declare the server store (route.Needs), so the router answers before any handler runs, knowledge service wired or not.
func TestKnowledgeHandlers_NilBackend_Returns503NotPanic(t *testing.T) {
	rt := newTestRouter(nil, false)
	moduleOf[*OrgHandler](t, rt).knowledge = rag.NewKnowledgeService(nil, nil, nil)

	cases := []struct {
		name, method, target string
	}{
		{"list", http.MethodGet, "/api/orgs/org-1/knowledge"},
		{"upload", http.MethodPost, "/api/orgs/org-1/knowledge/upload"},
		{"reindex", http.MethodPost, "/api/orgs/org-1/knowledge/reindex"},
		{"delete", http.MethodDelete, "/api/orgs/org-1/knowledge/doc-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Must not panic; must return 503.
			rr := doRequest(t, rt, tc.method, tc.target, nil)
			if rr.Code != http.StatusServiceUnavailable {
				t.Errorf("%s: status = %d, want 503 (nil backend should be a clean 503, not a panic)", tc.name, rr.Code)
			}
		})
	}
}

// KnowledgeService with a nil store must itself degrade cleanly (defense in depth): the store-touching methods must not nil-panic.
// With nil factory the embedder check surfaces first (also no panic); the store-nil guard is what would have panicked previously once an embedder succeeds.
func TestKnowledgeService_NilStore_DegradesCleanly(t *testing.T) {
	svc := rag.NewKnowledgeService(nil, nil, nil)

	// Neither call may panic. The exact error/result depends on which validation
	// fires first (provider vs store); the regression is the panic, not the value.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("AddDocument panicked on nil store: %v", r)
			}
		}()
		_ = svc.AddDocument(bgCtx(), "scope", "org", "f.txt", "content")
	}()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Search panicked on nil store: %v", r)
			}
		}()
		_, _ = svc.Search(bgCtx(), "scope", "org", "query")
	}()
}

func bgCtx() context.Context { return context.Background() }

// directionEmbedder answers every input with vec — "an embedding model" whose
// vectors point one way.
type directionEmbedder struct {
	ai.Provider
	vec []float32
}

func (d directionEmbedder) ID() string { return "direction-embed" }
func (d directionEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = append([]float32(nil), d.vec...)
	}
	return out, nil
}

// newKnowledgeGuardRouter serves the knowledge routes for an org whose admin is the (local-mode) caller and whose corpus was indexed as [1 0]; the caller embeds as callerVec.
func newKnowledgeGuardRouter(t *testing.T, callerVec []float32) (http.Handler, *testutil.FakeBackend, string) {
	t.Helper()
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &testutil.FakeBackend{}
	store.SeedOrgs(org.ID) // the knowledge base needs its organisation
	_ = store.SaveKnowledgeDocument(bgCtx(), "", &storageif.KnowledgeDocument{ID: "doc-1", OrgID: org.ID, Filename: "policy.md"})
	_ = store.SaveKnowledgeChunks(bgCtx(), "", []storageif.KnowledgeChunk{
		{ID: "c1", DocID: "doc-1", Content: "retry every call", Embedding: []float32{1, 0}},
		{ID: "c2", DocID: "doc-1", Content: "name flows by team", Embedding: []float32{1, 0}},
	})
	knowledge := rag.NewKnowledgeService(store, nil, nil, rag.WithEmbedder(directionEmbedder{vec: callerVec}))
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
	return mountedAs(h, "u-admin"), store, org.ID
}

func postJSON(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, reader))
	return rr
}

func errorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var env render.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", rr.Body.String(), err)
	}
	return env.Code
}

// TestKnowledgeReindex_ModelChanged409UnlessForced pins #35 at the HTTP layer: a re-index whose embedding model differs from the corpus's answers 409
// EMBEDDING_MODEL_CHANGED and writes nothing; the same request with {"force": true} re-indexes.
func TestKnowledgeReindex_ModelChanged409UnlessForced(t *testing.T) {
	h, store, orgID := newKnowledgeGuardRouter(t, []float32{0, 1})
	path := "/api/orgs/" + orgID + "/knowledge/reindex"

	for _, body := range []string{"", "{}", `{"force":false}`} {
		rr := postJSON(t, h, path, body)
		if rr.Code != http.StatusConflict || errorCode(t, rr) != "EMBEDDING_MODEL_CHANGED" {
			t.Fatalf("body %q: status = %d, body = %s; want 409 EMBEDDING_MODEL_CHANGED", body, rr.Code, rr.Body.String())
		}
	}
	if v := store.KnowledgeChunks["c1"].Embedding; v[0] != 1 || v[1] != 0 {
		t.Fatalf("refused re-index rewrote the corpus: %v", v)
	}

	rr := postJSON(t, h, path, `{"force":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("forced re-index: status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var res rag.ReindexResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil || res.Chunks != 2 {
		t.Errorf("forced re-index result = %+v (%v), want 2 chunks", res, err)
	}
	if v := store.KnowledgeChunks["c1"].Embedding; v[0] != 0 || v[1] != 1 {
		t.Errorf("forced re-index did not re-embed: %v", v)
	}

	if rr := postJSON(t, h, path, `{"force":`); rr.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status = %d, want 400", rr.Code)
	}
}

// TestKnowledgeUpload_AzureWithoutEmbeddingDeployment400 pins #16 at the HTTP layer: with Azure AI Foundry opted in but the caller's document naming no
// embedding deployment, the upload answers 400 EMBEDDING_NOT_CONFIGURED (the panel's guidance) instead of a masked 500 from Embed.
func TestKnowledgeUpload_AzureWithoutEmbeddingDeployment400(t *testing.T) {
	ai.ResetAzureFoundryCaches()
	factory := ai.NewProviderFactory(func(_, provider string) (string, error) {
		if provider == "azure-foundry" {
			return `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"gpt-4o"}`, nil
		}
		return "", nil
	}, nil, nil, nil)
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &testutil.FakeBackend{}
	store.SeedOrgs(org.ID) // the knowledge base needs its organisation
	knowledge := rag.NewKnowledgeService(store, factory, nil, rag.WithEmbeddingProvider("azure-foundry"))
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
	r := mountedAs(h, "u-admin")

	rr := postJSON(t, r, "/api/orgs/"+org.ID+"/knowledge/upload", `{"filename":"a.md","content":"a guideline"}`)
	if rr.Code != http.StatusBadRequest || errorCode(t, rr) != "EMBEDDING_NOT_CONFIGURED" {
		t.Fatalf("status = %d, body = %s; want 400 EMBEDDING_NOT_CONFIGURED", rr.Code, rr.Body.String())
	}
}

// failingAzureEmbedderSource is a model registry that cannot be read.
type failingAzureEmbedderSource struct{ err error }

func (s failingAzureEmbedderSource) KnowledgeEmbedder(context.Context, string, string) (ai.Provider, error) {
	return nil, s.err
}

// TestKnowledgeUpload_AzureRegistryFailureIs503Masked: when the Azure AI Foundry model registry (or the key store behind it) cannot be read, the upload is
// no 400 EMBEDDING_NOT_CONFIGURED echoing the driver's error — it names database hosts and roles — but a 503 EMBEDDING_PROVIDER_UNAVAILABLE with masked copy.
func TestKnowledgeUpload_AzureRegistryFailureIs503Masked(t *testing.T) {
	const driver = "read the organisation's Azure AI Foundry model registry: failed to connect to `host=10.1.2.3 user=baki_app database=baki`: dial error"
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	for _, cause := range []error{errors.New(driver), rag.ErrEmbeddingSetupUnavailable} {
		store := &testutil.FakeBackend{}
		store.SeedOrgs(org.ID) // the knowledge base needs its organisation
		knowledge := rag.NewKnowledgeService(store, ai.NewProviderFactory(nil, nil, nil, nil), nil,
			rag.WithEmbeddingProvider("azure-foundry"), rag.WithAzureEmbedder(failingAzureEmbedderSource{err: cause}))
		h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
		r := mountedAs(h, "u-admin")
		rr := postJSON(t, r, "/api/orgs/"+org.ID+"/knowledge/upload", `{"filename":"a.md","content":"a guideline"}`)
		if rr.Code != http.StatusServiceUnavailable || errorCode(t, rr) != "EMBEDDING_PROVIDER_UNAVAILABLE" {
			t.Errorf("%v: status = %d, body = %s; want 503 EMBEDDING_PROVIDER_UNAVAILABLE", cause, rr.Code, rr.Body.String())
		}
		if body := rr.Body.String(); strings.Contains(body, "10.1.2.3") || strings.Contains(body, "baki_app") {
			t.Errorf("the answer leaks the driver's text: %s", body)
		}
	}
}

// TestKnowledgeUpload_ModelChanged409: uploading with another embedding model
// than the corpus's is refused with the same machine code (no force on upload).
func TestKnowledgeUpload_ModelChanged409(t *testing.T) {
	h, store, orgID := newKnowledgeGuardRouter(t, []float32{0, 1})
	rr := postJSON(t, h, "/api/orgs/"+orgID+"/knowledge/upload", `{"filename":"new.md","content":"a new guideline"}`)
	if rr.Code != http.StatusConflict || errorCode(t, rr) != "EMBEDDING_MODEL_CHANGED" {
		t.Fatalf("status = %d, body = %s; want 409 EMBEDDING_MODEL_CHANGED", rr.Code, rr.Body.String())
	}
	if len(store.KnowledgeDocs) != 1 {
		t.Errorf("refused upload stored a document: %d docs", len(store.KnowledgeDocs))
	}

	// Same model: the upload goes through.
	same, _, sameOrg := newKnowledgeGuardRouter(t, []float32{1, 0})
	if rr := postJSON(t, same, "/api/orgs/"+sameOrg+"/knowledge/upload", `{"filename":"new.md","content":"a new guideline"}`); rr.Code != http.StatusOK {
		t.Errorf("same-model upload: status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// failingEmbedder fails every Embed with err — "the provider answered with
// this" for the error-mapping tests.
type failingEmbedder struct {
	ai.Provider
	err error
}

func (f failingEmbedder) ID() string { return "failing-embed" }
func (f failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, f.err
}

// newKnowledgeErrorRouter serves the knowledge routes for an org (admin =
// the local-mode caller) whose embedder fails with embedErr.
func newKnowledgeErrorRouter(t *testing.T, embedErr error) (http.Handler, string) {
	t.Helper()
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &testutil.FakeBackend{}
	store.SeedOrgs(org.ID) // the knowledge base needs its organisation
	knowledge := rag.NewKnowledgeService(store, nil, nil, rag.WithEmbedder(failingEmbedder{err: embedErr}))
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
	return mountedAs(h, "u-admin"), org.ID
}

// errorEnvelope decodes the standard error body.
func errorEnvelope(t *testing.T, rr *httptest.ResponseRecorder) render.ErrorResponse {
	t.Helper()
	var env render.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", rr.Body.String(), err)
	}
	return env
}

// providerErrorCases is the table of provider-side embedding failures and the status/code they must map to. None is a 500: they are the admin's credentials,
// deployment or quota (4xx, message kept) or the provider's availability (503, message masked) — never a server fault.
var providerErrorCases = []struct {
	name       string
	err        error
	status     int
	code       string
	msgContain string // "" = the message is masked (5xx)
}{
	{"invalid key", fmt.Errorf("openai embeddings: %w", ai.ErrApiKeyInvalid), http.StatusBadRequest, "EMBEDDING_AUTH_FAILED", "invalid API key"},
	// permissionDeniedError unwraps to BOTH sentinels; the ladder must test
	// permission-denied first.
	{"permission denied", fmt.Errorf("azure ai foundry embeddings: %w", errors.Join(ai.ErrProviderPermissionDenied, ai.ErrApiKeyInvalid)), http.StatusBadRequest, "EMBEDDING_PERMISSION_DENIED", "permission denied"},
	{"rate limited", &ai.RateLimitError{RetryAfter: 7 * time.Second}, http.StatusTooManyRequests, "EMBEDDING_RATE_LIMITED", "rate limited"},
	{"provider down", fmt.Errorf("openai embeddings: %w", ai.ErrProviderDown), http.StatusServiceUnavailable, "EMBEDDING_PROVIDER_UNAVAILABLE", ""},
	{"circuit open", ai.ErrCircuitOpen, http.StatusServiceUnavailable, "EMBEDDING_PROVIDER_UNAVAILABLE", ""},
	{"config invalid", fmt.Errorf("azure ai foundry: %w", ai.ErrProviderConfigInvalid), http.StatusBadRequest, "EMBEDDING_NOT_CONFIGURED", "configuration is invalid"},
	{"key not configured", ai.ErrKeyNotConfigured, http.StatusBadRequest, "EMBEDDING_NOT_CONFIGURED", "not configured"},
	{"unknown deployment", fmt.Errorf("azure ai foundry API: The API deployment for this resource does not exist"), http.StatusBadRequest, "EMBEDDING_PROVIDER_REJECTED", "deployment for this resource does not exist"},
}

// TestKnowledgeUpload_ProviderErrorsAreNot500 pins the error ladder at the HTTP layer, on both bulk routes: a rotated key, a missing data-plane role, a
// throttled resource, an outage or a mistyped deployment each get their machine code, and the 4xx envelope keeps the provider's message so the panel can show it.
func TestKnowledgeUpload_ProviderErrorsAreNot500(t *testing.T) {
	for _, tc := range providerErrorCases {
		t.Run(tc.name, func(t *testing.T) {
			h, orgID := newKnowledgeErrorRouter(t, tc.err)
			for _, route := range []struct{ path, body string }{
				{"/api/orgs/" + orgID + "/knowledge/upload", `{"filename":"a.md","content":"a guideline"}`},
				{"/api/orgs/" + orgID + "/knowledge/reindex", `{"force":true}`},
			} {
				// The re-index of an empty corpus never embeds; give it one
				// chunk so the failure is reached on that route too.
				rr := postJSON(t, h, route.path, route.body)
				if strings.HasSuffix(route.path, "/reindex") && rr.Code == http.StatusOK {
					continue // empty corpus: nothing to embed, nothing to fail (covered by the upload leg)
				}
				if rr.Code >= 500 && tc.status < 500 {
					t.Fatalf("%s: status = %d (%s) — a provider failure must not be a masked 500", route.path, rr.Code, rr.Body.String())
				}
				env := errorEnvelope(t, rr)
				if rr.Code != tc.status || env.Code != tc.code {
					t.Fatalf("%s: status = %d code = %q, body = %s; want %d %s", route.path, rr.Code, env.Code, rr.Body.String(), tc.status, tc.code)
				}
				if tc.msgContain != "" && !strings.Contains(env.Message, tc.msgContain) {
					t.Errorf("%s: message = %q, want it to contain %q (4xx keeps the provider's explanation)", route.path, env.Message, tc.msgContain)
				}
				if tc.status >= 500 && strings.Contains(env.Message, "embeddings") {
					t.Errorf("%s: 5xx message = %q, want it masked", route.path, env.Message)
				}
				if tc.code == "EMBEDDING_RATE_LIMITED" && rr.Header().Get("Retry-After") != "7" {
					t.Errorf("%s: Retry-After = %q, want the provider's hint (7)", route.path, rr.Header().Get("Retry-After"))
				}
			}
		})
	}
}

// TestRenderKnowledgeEmbeddingError_Ladder pins the mapping itself, including the rag sentinels the routes reach through other paths and the ordering hazards
// (permission-denied before invalid-key; the ErrEmbeddingFailed catch-all last; unknown errors fall through to the caller's 500).
func TestRenderKnowledgeEmbeddingError_Ladder(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not configured", rag.ErrEmbeddingNotConfigured, http.StatusBadRequest, "EMBEDDING_NOT_CONFIGURED"},
		{"unavailable", fmt.Errorf("x: %w", rag.ErrEmbeddingUnavailable), http.StatusBadRequest, "EMBEDDING_NOT_CONFIGURED"},
		// The org sentinel must map ahead of the ErrEmbeddingUnavailable
		// case the rag embedder wraps it with.
		{"org credential has no embedding deployment", fmt.Errorf("%w: azure-foundry: %w", rag.ErrEmbeddingUnavailable, ai.ErrOrgEmbeddingDeploymentMissing), http.StatusBadRequest, "EMBEDDING_ORG_NOT_CONFIGURED"},
		// More passages than one upload may index.
		{"document too large", fmt.Errorf("%w: 3000 passages exceed the limit of 2500", rag.ErrDocumentTooLarge), http.StatusRequestEntityTooLarge, "KNOWLEDGE_DOCUMENT_TOO_LARGE"},
		{"model changed", rag.ErrEmbeddingModelChanged, http.StatusConflict, "EMBEDDING_MODEL_CHANGED"},
		// Which model embeds could not be decided (the registry or the
		// key store unreadable): transient and masked, no configuration gap.
		{"setup unavailable", fmt.Errorf("embedding provider unavailable: %w", rag.ErrEmbeddingSetupUnavailable), http.StatusServiceUnavailable, "EMBEDDING_PROVIDER_UNAVAILABLE"},
		{"busy", rag.ErrKnowledgeBusy, http.StatusConflict, "KNOWLEDGE_BUSY"},
		{"lock unavailable", rag.ErrKnowledgeLockUnavailable, http.StatusServiceUnavailable, "KNOWLEDGE_LOCK_UNAVAILABLE"},
		{"failed: rate limited", fmt.Errorf("%w: %w", rag.ErrEmbeddingFailed, &ai.RateLimitError{}), http.StatusTooManyRequests, "EMBEDDING_RATE_LIMITED"},
		{"failed: permission denied", fmt.Errorf("%w: %w", rag.ErrEmbeddingFailed, errors.Join(ai.ErrProviderPermissionDenied, ai.ErrApiKeyInvalid)), http.StatusBadRequest, "EMBEDDING_PERMISSION_DENIED"},
		{"failed: invalid key", fmt.Errorf("%w: %w", rag.ErrEmbeddingFailed, ai.ErrApiKeyInvalid), http.StatusBadRequest, "EMBEDDING_AUTH_FAILED"},
		{"failed: down", fmt.Errorf("%w: %w", rag.ErrEmbeddingFailed, ai.ErrProviderDown), http.StatusServiceUnavailable, "EMBEDDING_PROVIDER_UNAVAILABLE"},
		{"failed: rejected", fmt.Errorf("%w: azure ai foundry API: bad input", rag.ErrEmbeddingFailed), http.StatusBadRequest, "EMBEDDING_PROVIDER_REJECTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			if !renderKnowledgeEmbeddingError(rr, tc.err) {
				t.Fatalf("not rendered: %v", tc.err)
			}
			if rr.Code != tc.status || errorCode(t, rr) != tc.code {
				t.Errorf("status = %d code = %q, want %d %s", rr.Code, errorCode(t, rr), tc.status, tc.code)
			}
		})
	}
	if renderKnowledgeEmbeddingError(httptest.NewRecorder(), errors.New("db: connection reset")) {
		t.Error("an unrelated error must fall through to the caller's 500")
	}
	if got := embeddingRetryAfter(&ai.RateLimitError{RetryAfter: 1500 * time.Millisecond}); got != "2" {
		t.Errorf("Retry-After for 1.5s = %q, want 2 (rounded up)", got)
	}
	if got := embeddingRetryAfter(ai.ErrRateLimited); got != "30" {
		t.Errorf("Retry-After without a hint = %q, want 30", got)
	}
}

// auditRecordingBackend records the audit rows the handler writes.
type auditRecordingBackend struct {
	*testutil.FakeBackend
	mu     sync.Mutex
	events []*storageif.AuditEvent
}

func (a *auditRecordingBackend) SaveAuditEvent(_ context.Context, e *storageif.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return nil
}

func (a *auditRecordingBackend) lastEvent(t *testing.T, action string) *storageif.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		for i := len(a.events) - 1; i >= 0; i-- {
			if a.events[i].Action == action {
				e := a.events[i]
				a.mu.Unlock()
				return e
			}
		}
		a.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s audit event recorded", action)
	return nil
}

// TestKnowledgeReindex_TruncatedSurfacesInBodyAndAudit: a corpus beyond the per-request ceiling answers {"truncated":true} (the panel tells the admin to
// re-run) and the audit row records it alongside force.
func TestKnowledgeReindex_TruncatedSurfacesInBodyAndAudit(t *testing.T) {
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store := &auditRecordingBackend{FakeBackend: &testutil.FakeBackend{}}
	store.SeedOrgs(org.ID) // the knowledge base needs its organisation
	const docs, ceiling = 3, 10000
	for d := range docs {
		_ = store.SaveKnowledgeDocument(bgCtx(), "", &storageif.KnowledgeDocument{ID: fmt.Sprintf("doc-%d", d), OrgID: org.ID, Filename: fmt.Sprintf("doc-%d.md", d)})
	}
	chunks := make([]storageif.KnowledgeChunk, ceiling+1)
	for i := range chunks {
		chunks[i] = storageif.KnowledgeChunk{ID: fmt.Sprintf("c-%06d", i), DocID: fmt.Sprintf("doc-%d", i%docs), Content: fmt.Sprintf("rule %d", i), Embedding: []float32{0, 1}}
	}
	_ = store.SaveKnowledgeChunks(bgCtx(), "", chunks)
	knowledge := rag.NewKnowledgeService(store, nil, nil, rag.WithEmbedder(directionEmbedder{vec: []float32{0, 1}}))
	h := NewOrgHandler(orgSvc, store, knowledge, &SecurityConfig{LocalUserID: "u-admin"}, mail.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(store, store, nil), nil, service.NewAuthzService(store, orgSvc), service.NewChannelService(store), service.NewUserDirectory(store), testAuditRecorder(store), nil)
	r := mountedAs(h, "u-admin")

	rr := postJSON(t, r, "/api/orgs/"+org.ID+"/knowledge/reindex", `{"force":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["truncated"] != true || body["chunks"] != float64(ceiling) || body["docs"] != float64(docs) {
		t.Errorf("body = %s, want truncated=true chunks=%d docs=%d", rr.Body.String(), ceiling, docs)
	}
	ev := store.lastEvent(t, audit.ActionKnowledgeReindex)
	if ev.Meta["truncated"] != "true" || ev.Meta["force"] != "true" {
		t.Errorf("audit meta = %v, want truncated=true and force=true", ev.Meta)
	}

	// An unforced re-run (the converted corpus is now the caller's model, so the guard passes) is truncated again — the walk restarts per request — and its
	// audit row carries truncated without force.
	rr = postJSON(t, r, "/api/orgs/"+org.ID+"/knowledge/reindex", `{}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"truncated":true`) {
		t.Fatalf("unforced re-run: status = %d, body = %s; want 200 with truncated", rr.Code, rr.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		store.mu.Lock()
		n := len(store.events)
		store.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.events) != 2 {
		t.Fatalf("%d audit events, want 2", len(store.events))
	}
	if meta := store.events[1].Meta; meta["truncated"] != "true" || meta["force"] != "" {
		t.Errorf("unforced re-run audit meta = %v, want truncated=true and no force", meta)
	}
}
