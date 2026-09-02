package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
)

// newNilBackendOrgHandler builds an OrgHandler whose backend is nil (local mode)
// but whose knowledge service is non-nil — the exact configuration that
// previously nil-panicked in handleKnowledgeList/handleKnowledgeDelete because
// the handlers dereferenced h.backend after only checking the (non-nil) service.
func newNilBackendOrgHandler() *OrgHandler {
	security := &SecurityConfig{}
	orgSvc := collaboration.NewOrgService(nil) // unused — the guards return before reaching it
	return NewOrgHandler(orgSvc, nil /*backend*/, rag.NewKnowledgeService(nil, nil, nil), security, mail.NewService(config.EmailConfig{}, config.ModeLocal))
}

// Regression (H8/Track 6): the Knowledge Base handlers must return 503 when the
// storage backend is nil (local mode) instead of nil-panicking on h.backend.
// The latent bug: the service is constructed non-nil even with a nil store, so
// the `h.knowledge == nil` guard did not protect the h.backend dereference.
func TestKnowledgeHandlers_NilBackend_Returns503NotPanic(t *testing.T) {
	h := newNilBackendOrgHandler()

	cases := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		target  string
	}{
		{"list", h.handleKnowledgeList, http.MethodGet, "/api/orgs/org-1/knowledge"},
		{"upload", h.handleKnowledgeUpload, http.MethodPost, "/api/orgs/org-1/knowledge/upload"},
		{"reindex", h.handleKnowledgeReindex, http.MethodPost, "/api/orgs/org-1/knowledge/reindex"},
		{"delete", h.handleKnowledgeDelete, http.MethodDelete, "/api/orgs/org-1/knowledge/doc-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(tc.method, tc.target, nil)
			// Must not panic; must return 503.
			tc.handler(rr, r)
			if rr.Code != http.StatusServiceUnavailable {
				t.Errorf("%s: status = %d, want 503 (nil backend should be a clean 503, not a panic)", tc.name, rr.Code)
			}
		})
	}
}

// KnowledgeService with a nil store must itself degrade cleanly (defense in
// depth): the store-touching methods must not nil-panic. With nil factory the
// embedder check surfaces first (also no panic); the store-nil guard is what
// would have panicked previously once an embedder succeeds.
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
