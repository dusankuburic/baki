package api

import (
	"net/http"
	"testing"
)

// --- No-storage (nil backend) baseline tests ---

func TestHandleLibraryCreate_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/library")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleLibraryGet_NoStorageReturns404(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodGet, "/api/library/some-id", nil)
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleLibraryDelete_NoStorageReturns404(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodDelete, "/api/library/some-id", nil)
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleLibrary_UnknownMethodReturns405(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPatch, "/api/library", nil)
	checkStatus(t, rr, http.StatusMethodNotAllowed)
}

// --- JWT + filesystem backend ownership tests ---

func TestHandleLibraryGet_OwnerCanRead(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/flow1", bearer, nil)
	checkStatus(t, rr, http.StatusOK)

	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["id"] != "flow1" {
		t.Errorf("expected id=flow1, got %v", resp["id"])
	}
	if resp["isSharedWithMe"] != false {
		t.Errorf("expected isSharedWithMe=false for owner, got %v", resp["isSharedWithMe"])
	}
}

func TestHandleLibraryGet_NonOwnerForbidden(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/flow1", bearer, nil)
	checkStatus(t, rr, http.StatusForbidden)
}

func TestHandleLibraryGet_NonexistentReturns404(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/no-such-flow", bearer, nil)
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleLibraryDelete_OwnerCanDelete(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/library/flow1", bearer, nil)
	checkStatus(t, rr, http.StatusOK)

	// Second delete should return 404 (already gone).
	rr2 := doRequestWithAuth(t, rt, http.MethodDelete, "/api/library/flow1", bearer, nil)
	checkStatus(t, rr2, http.StatusNotFound)
}

func TestHandleLibraryDelete_NonOwnerForbidden(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/library/flow1", bearer, nil)
	checkStatus(t, rr, http.StatusForbidden)
}

func TestHandleLibraryGet_SharedFlowVisibleToNonOwner(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	// Seeding with empty OwnerID makes the flow visible to everyone (legacy compatibility).
	seed("flow-public", "")
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/flow-public", bearer, nil)
	// Empty OwnerID means the ownership check is skipped (doc.OwnerID == "").
	checkStatus(t, rr, http.StatusOK)
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["isSharedWithMe"] != true {
		t.Errorf("expected isSharedWithMe=true for non-owner, got %v", resp["isSharedWithMe"])
	}
}

func TestHandleLibraryGet_ResolvesOwnerDisplayName(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seedUser(t, rt, "alice", "alice@example.com")
	seed("flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/flow1", bearer, nil)
	checkStatus(t, rr, http.StatusOK)

	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["ownerDisplayName"] != "alice@example.com" {
		t.Errorf("expected ownerDisplayName=alice@example.com, got %v", resp["ownerDisplayName"])
	}
}

func TestHandleLibraryUpdate_EmptyOwnerForbidden(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	// Legacy flow with no owner must not be world-writable in cloud mode.
	seed("flow-public", "")
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/flow-public", bearer, map[string]any{
		"name": "hijacked",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestHandleLibraryUpdate_OwnerCanUpdate(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/flow1", bearer, map[string]any{
		"name": "renamed",
	})
	checkStatus(t, rr, http.StatusOK)
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["name"] != "renamed" {
		t.Errorf("expected name=renamed, got %v", resp["name"])
	}
}
