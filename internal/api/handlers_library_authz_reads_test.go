package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// collabCountingBackend is a FakeBackend with one editor collaborator on
// every flow and a count of the collaborator reads a request costs.
type collabCountingBackend struct {
	*testutil.FakeBackend
	collaborator string
	listCollabs  atomic.Int64
}

func (b *collabCountingBackend) ListCollaborators(context.Context, string, int) ([]*storageif.Collaborator, error) {
	b.listCollabs.Add(1)
	return []*storageif.Collaborator{{UserID: b.collaborator, Email: b.collaborator + "@example.com", Permission: "editor", GrantedAt: time.Now().UTC()}}, nil
}

// newCollabLibraryRouter is a JWT-mode router over collabCountingBackend
// with flowID owned by alice and shared with dave as an editor; it returns
// dave's bearer.
func newCollabLibraryRouter(t *testing.T, flowID string) (*Router, *collabCountingBackend, string) {
	t.Helper()
	backend := &collabCountingBackend{FakeBackend: testutil.NewFakeBackend(), collaborator: "dave"}
	backend.Flows[flowID] = &storageif.FlowDocument{ID: flowID, Name: "shared", OwnerID: "alice", Content: []byte(`{}`)}
	rt := newTestRouter(backend, true)
	return rt, backend, jwtBearer(t, rt, "dave", "dave@example.com")
}

// TestLibraryGet_OneAuthzReadPerRequest: as a collaborator, GET
// /api/library/{id} reads the flow's collaborators once — the read check
// and the response's capability flags share it (it used to be read twice).
func TestLibraryGet_OneAuthzReadPerRequest(t *testing.T) {
	rt, backend, bearer := newCollabLibraryRouter(t, "flow-memo-get")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/flow-memo-get", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var out libraryFlow
	decodeJSON(t, rr, &out)
	if !out.CanEdit || out.CanShare {
		t.Fatalf("editor collaborator flags = edit:%v share:%v, want edit only", out.CanEdit, out.CanShare)
	}
	if n := backend.listCollabs.Load(); n != 1 {
		t.Fatalf("GET read the collaborators %d times; want exactly 1", n)
	}
}

// TestLibraryUpdate_OneAuthzReadPerRequest: as a collaborator, PUT
// /api/library/{id} reads the flow's collaborators once for its three
// decisions (read gate, write gate, capability flags) — inside the
// request's write transaction, where it used to read them three times.
func TestLibraryUpdate_OneAuthzReadPerRequest(t *testing.T) {
	rt, backend, bearer := newCollabLibraryRouter(t, "flow-memo-put")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/flow-memo-put", bearer, map[string]any{"name": "renamed"})
	checkStatus(t, rr, http.StatusOK)
	var out libraryFlow
	decodeJSON(t, rr, &out)
	if out.Name != "renamed" || !out.CanEdit {
		t.Fatalf("response = name:%q edit:%v, want renamed and editable", out.Name, out.CanEdit)
	}
	if n := backend.listCollabs.Load(); n != 1 {
		t.Fatalf("PUT read the collaborators %d times; want exactly 1", n)
	}
}
