package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// contentCountingBackend counts CONTENT loads (LoadFlow) so tests can prove a
// permission-only endpoint never downloads flow content.
type contentCountingBackend struct {
	*testutil.FakeBackend
	contentLoads atomic.Int64
}

func (b *contentCountingBackend) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	b.contentLoads.Add(1)
	return b.FakeBackend.LoadFlow(ctx, id)
}

// TestPermOnlyEndpoints_NeverLoadFlowContent is the gate for header-first
// authz: triage/comments/sharing permission checks must decide from the flow
// HEADER alone. Previously each check called GetAuthorized, which downloaded
// and unmarshaled the entire flow (blob round-trip per request) and threw the
// document away.
func TestPermOnlyEndpoints_NeverLoadFlowContent(t *testing.T) {
	backend := &contentCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlowForAuthz(t, backend.FakeBackend)
	rt := newTestRouter(backend, true)
	tok := jwtBearer(t, rt, "u1", "u1@example.com")

	// A perm-only endpoint (triage list): 200 with zero content loads.
	req := httptest.NewRequest(http.MethodPost, "/api/analysis/triage/list", nil)
	req.Header.Set("Authorization", tok)
	req.Header.Set("Content-Type", "application/json")
	req.Body = http.NoBody
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("triage/list returned 500: %s", rec.Body.String())
	}
	if n := backend.contentLoads.Load(); n != 0 {
		t.Errorf("triage/list loaded flow CONTENT %d times; header-only authz must not download content", n)
	}

	// Denied caller: still zero content loads (the decision precedes any
	// content resolution).
	other := jwtBearer(t, rt, "intruder", "intruder@example.com")
	req2 := httptest.NewRequest(http.MethodPost, "/api/analysis/triage/list", nil)
	req2.Header.Set("Authorization", other)
	req2.Header.Set("Content-Type", "application/json")
	req2.Body = http.NoBody
	rec2 := httptest.NewRecorder()
	rt.ServeHTTP(rec2, req2)
	if n := backend.contentLoads.Load(); n != 0 {
		t.Errorf("denied triage/list loaded flow CONTENT %d times; denial must cost only a header query", n)
	}
}

func seedCloudFlowForAuthz(t *testing.T, backend *testutil.FakeBackend) {
	t.Helper()
	if err := backend.SaveFlow(context.Background(), &interfaces.FlowDocument{
		ID:      "f1",
		Name:    "Flow One",
		OwnerID: "u1",
		Content: []byte(`{"id":"f1","name":"Flow One","subflows":[]}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}
