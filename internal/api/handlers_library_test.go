package api

import (
	"net/http"
	"testing"
)

// --- No-storage (nil backend) baseline tests ---

func TestHandleLibraryList_NoStorage_ReturnsEmptyArray(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/library", nil)
	checkStatus(t, rr, http.StatusOK)

	var result []any
	decodeJSON(t, rr, &result)
	if len(result) != 0 {
		t.Errorf("expected empty array, got %d items", len(result))
	}
}

func TestHandleLibraryCreate_MissingNameReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/library", map[string]any{
		"description": "no name here",
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleLibraryCreate_NoStorageReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/library", map[string]any{
		"name": "My Flow",
	})
	checkStatus(t, rr, http.StatusInternalServerError)
}

func TestHandleLibraryCreate_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/library")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleLibraryGet_NoStorageReturns404(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/library/some-id", nil)
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleLibraryDelete_NoStorageReturns404(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodDelete, "/api/library/some-id", nil)
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleLibraryItem_UnknownMethodReturns404(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPut, "/api/library/some-id", nil)
	checkStatus(t, rr, http.StatusNotFound)
}

func TestHandleLibrary_UnknownMethodReturns404(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPut, "/api/library", nil)
	checkStatus(t, rr, http.StatusNotFound)
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
