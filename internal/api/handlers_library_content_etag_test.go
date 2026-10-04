package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// getContent performs GET /api/library/{id}/content with optional
// If-None-Match and returns the recorder.
func getContent(t *testing.T, rt *Router, bearer, flowID, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/library/"+flowID+"/content", nil)
	req.Header.Set("Authorization", bearer)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}

// TestLibraryContent_ETagRevalidation: the content endpoint tags the body with
// the flow's version; a client holding the current version gets a 304 without
// a content read (the header alone decides), a save invalidates the tag, and
// a caller without read access never gets a 304 either.
func TestLibraryContent_ETagRevalidation(t *testing.T) {
	rt, b := newReadCountingRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	alice := jwtBearer(t, rt, "alice", "alice@example.com")

	first := getContent(t, rt, alice, "flow1", "")
	checkStatus(t, first, http.StatusOK)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("content response carries no ETag")
	}
	if cc := first.Header().Get("Cache-Control"); cc != "private, no-cache" {
		t.Errorf("Cache-Control = %q, want private, no-cache (browser must revalidate)", cc)
	}
	if first.Header().Get("X-Flow-Version") == "" {
		t.Error("X-Flow-Version header dropped")
	}
	loads := b.loadFlow

	// Same version: 304, no content read, same tag echoed.
	notModified := getContent(t, rt, alice, "flow1", etag)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match with the current tag: status %d, want 304 (body %s)", notModified.Code, notModified.Body.String())
	}
	if notModified.Body.Len() != 0 {
		t.Errorf("304 carried a body of %d bytes", notModified.Body.Len())
	}
	if b.loadFlow != loads {
		t.Errorf("304 read the content %d times (header must decide)", b.loadFlow-loads)
	}
	if got := notModified.Header().Get("ETag"); got != etag {
		t.Errorf("304 ETag = %q, want %q", got, etag)
	}

	// Weak tags and lists are honoured.
	if rr := getContent(t, rt, alice, "flow1", `"other", W/`+etag); rr.Code != http.StatusNotModified {
		t.Errorf("list with weak match: status %d, want 304", rr.Code)
	}
	// A stale tag gets the body.
	if rr := getContent(t, rt, alice, "flow1", `"flow1-v999"`); rr.Code != http.StatusOK {
		t.Errorf("stale tag: status %d, want 200", rr.Code)
	}

	// A save bumps the version: the old tag no longer matches and the new
	// response carries a new one.
	body, _ := json.Marshal(map[string]any{
		"name": "flow1", "content": map[string]any{"id": "flow1", "name": "flow1", "subflows": []any{}}, "version": b.versionOf(t, "flow1"),
	})
	req := httptest.NewRequest(http.MethodPut, "/api/library/flow1", bytes.NewReader(body))
	req.Header.Set("Authorization", alice)
	req.Header.Set("Content-Type", "application/json")
	put := httptest.NewRecorder()
	rt.ServeHTTP(put, req)
	checkStatus(t, put, http.StatusOK)

	after := getContent(t, rt, alice, "flow1", etag)
	checkStatus(t, after, http.StatusOK)
	if got := after.Header().Get("ETag"); got == "" || got == etag {
		t.Errorf("ETag after save = %q, want a new tag (old %q)", got, etag)
	}

	// No read access: 403, with or without a matching tag.
	bob := jwtBearer(t, rt, "bob", "bob@example.com")
	if rr := getContent(t, rt, bob, "flow1", after.Header().Get("ETag")); rr.Code != http.StatusForbidden {
		t.Errorf("non-reader with a matching tag: status %d, want 403", rr.Code)
	}
}

// versionOf reads a flow's current version through the backend.
func (b *readCountingBackend) versionOf(t *testing.T, id string) int {
	t.Helper()
	hdr, err := b.LoadFlowHeader(t.Context(), id)
	if err != nil {
		t.Fatalf("load header: %v", err)
	}
	return hdr.Version
}
