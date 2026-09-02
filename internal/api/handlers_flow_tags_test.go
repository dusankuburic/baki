package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// newTagsTestRouter builds a JWT router on the FakeBackend (tags are
// cloud-library features; the filesystem backend intentionally rejects them)
// with one editor user and one seeded org flow.
func newTagsTestRouter(t *testing.T) (*Router, *testutil.FakeBackend, string) {
	t.Helper()
	backend := &testutil.FakeBackend{}
	rt := newTestRouter(backend, true)
	seedUserWithRole(t, rt, "editor", "editor@example.com", auth.RoleMember)
	doc := &storageif.FlowDocument{ID: "flow-1", Name: "F", OwnerID: "editor", OrganizationID: ""}
	if err := backend.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	return rt, backend, doc.ID
}

// TestFlowTags_SetAndValidate pins R2-4's endpoint: normalization on write
// (the response carries the canonical set), invalid tags rejected with 400,
// and the stored set round-trips through UpdateFlowTags.
func TestFlowTags_SetAndValidate(t *testing.T) {
	rt, backend, flowID := newTagsTestRouter(t)
	bearer := jwtBearer(t, rt, "editor", "editor@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/flow/tags", bearer,
		map[string]any{"flowId": flowID, "tags": []string{"  PROD ", "prod", "Business-Unit"}})
	checkStatus(t, rr, http.StatusOK)
	var res struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Tags) != 2 || res.Tags[0] != "prod" || res.Tags[1] != "business-unit" {
		t.Errorf("normalized tags = %v", res.Tags)
	}
	if got := backend.FlowTags[flowID]; len(got) != 2 || got[0] != "prod" {
		t.Errorf("stored tags = %v", got)
	}

	// Invalid tag name → 400 (name the tag for the UI).
	rr = doRequestWithAuth(t, rt, http.MethodPut, "/api/flow/tags", bearer,
		map[string]any{"flowId": flowID, "tags": []string{"has space"}})
	checkStatus(t, rr, http.StatusBadRequest)
}

// TestFlowTags_ViewerForbidden: tags change governance/filters — editor only.
func TestFlowTags_ViewerForbidden(t *testing.T) {
	rt, _, flowID := newTagsTestRouter(t)
	seedUserWithRole(t, rt, "viewer", "viewer@example.com", auth.RoleMember)
	bearer := jwtBearer(t, rt, "viewer", "viewer@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/flow/tags", bearer,
		map[string]any{"flowId": flowID, "tags": []string{"prod"}})
	checkStatus(t, rr, http.StatusForbidden)
}
