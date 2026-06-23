package api

import (
	"context"
	"net/http"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// --- Handler-level OCC tests (filesystem backend) ---

func TestLibraryUpdate_ResponseIncludesVersion(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("vflow", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/vflow", bearer, map[string]any{
		"name": "updated",
	})
	checkStatus(t, rr, http.StatusOK)

	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if _, ok := resp["version"]; !ok {
		t.Error("response missing 'version' field")
	}
}

func TestLibraryUpdate_VersionIncrementsOnSuccessiveSaves(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("vflow2", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// First update — seed creates flow at version=0, so version=0 is allowed.
	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/vflow2", bearer, map[string]any{
		"name":    "v1",
		"version": 0,
	})
	checkStatus(t, rr, http.StatusOK)
	var resp1 libraryFlow
	decodeJSON(t, rr, &resp1)

	// Second update — must send the version from the first response for OCC.
	rr2 := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/vflow2", bearer, map[string]any{
		"name":    "v2",
		"version": resp1.Version,
	})
	checkStatus(t, rr2, http.StatusOK)
	var resp2 libraryFlow
	decodeJSON(t, rr2, &resp2)

	if resp2.Version <= resp1.Version {
		t.Errorf("version should increment: first=%d second=%d", resp1.Version, resp2.Version)
	}
}

func TestLibraryUpdate_VersionZeroRejectedOnExistingFlow(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("vflow3", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// First update bumps the version from 0 to 1.
	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/vflow3", bearer, map[string]any{
		"name": "v1", "version": 0,
	})
	checkStatus(t, rr, http.StatusOK)

	// Second update with version=0 must be rejected — the client must send
	// the version they loaded to participate in OCC.
	rr2 := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/vflow3", bearer, map[string]any{
		"name": "v2", "version": 0,
	})
	checkStatus(t, rr2, http.StatusConflict)
}

func TestLibraryGetContent_ReturnsVersionHeader(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("hdrflow", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/hdrflow/content", bearer, nil)
	checkStatus(t, rr, http.StatusOK)

	if v := rr.Header().Get("X-Flow-Version"); v == "" {
		t.Error("X-Flow-Version header missing")
	}
}

func TestLibraryUpdate_NotifierCalledAfterSave(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("nflow", "alice")

	notifier := &captureNotifier{}
	rt.handlers.Library.SetFlowNotifier(notifier)

	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/nflow", bearer, map[string]any{
		"name": "notified",
	})
	checkStatus(t, rr, http.StatusOK)

	if !notifier.called {
		t.Error("expected NotifyFlowChanged to be called after save")
	}
}

// --- Storage-level OCC tests (FakeBackend) ---

func TestFakeBackend_OCC_NewFlowVersionZero(t *testing.T) {
	b := testutil.NewFakeBackend()
	doc := &storageif.FlowDocument{ID: "f1", Name: "test"}

	if err := b.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	if doc.Version != 0 {
		t.Errorf("new flow version: want 0, got %d", doc.Version)
	}
}

func TestFakeBackend_OCC_UpdateIncrementsVersion(t *testing.T) {
	b := testutil.NewFakeBackend()
	ctx := context.Background()

	doc := &storageif.FlowDocument{ID: "f2", Name: "test"}
	_ = b.SaveFlow(ctx, doc) // version=0

	doc.Name = "v2"
	doc.Version = 0 // correct expected version
	if err := b.SaveFlow(ctx, doc); err != nil {
		t.Fatalf("SaveFlow update: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("after first update: want version 1, got %d", doc.Version)
	}

	doc.Name = "v3"
	doc.Version = 1 // correct expected version
	if err := b.SaveFlow(ctx, doc); err != nil {
		t.Fatalf("SaveFlow update 2: %v", err)
	}
	if doc.Version != 2 {
		t.Errorf("after second update: want version 2, got %d", doc.Version)
	}
}

func TestFakeBackend_OCC_StaleVersionConflict(t *testing.T) {
	b := testutil.NewFakeBackend()
	ctx := context.Background()

	doc := &storageif.FlowDocument{ID: "f3", Name: "test"}
	_ = b.SaveFlow(ctx, doc) // version=0
	doc.Version = 0
	_ = b.SaveFlow(ctx, doc) // version=1

	// Try to save with stale version=0 when DB has version=1
	// But version=0 skips the check! So this should succeed.
	// To test real conflict, we need version > 0 that doesn't match.
	stale := &storageif.FlowDocument{ID: "f3", Name: "stale", Version: 99}
	err := b.SaveFlow(ctx, stale)
	if err == nil {
		t.Fatal("expected ErrVersionConflict for stale version 99, got nil")
	}
}

func TestFakeBackend_OCC_VersionZeroSkipsCheck(t *testing.T) {
	b := testutil.NewFakeBackend()
	ctx := context.Background()

	doc := &storageif.FlowDocument{ID: "f4", Name: "test"}
	_ = b.SaveFlow(ctx, doc) // version=0
	doc.Version = 0
	_ = b.SaveFlow(ctx, doc) // version=1
	doc.Version = 0          // version=0 should skip check even though DB has 1
	doc.Name = "force overwrite"
	err := b.SaveFlow(ctx, doc)
	if err != nil {
		t.Errorf("version=0 should skip check: %v", err)
	}
	if doc.Version != 2 {
		t.Errorf("after forced update: want version 2, got %d", doc.Version)
	}
}

func TestFakeBackend_OCC_CorrectVersionSucceeds(t *testing.T) {
	b := testutil.NewFakeBackend()
	ctx := context.Background()

	doc := &storageif.FlowDocument{ID: "f5", Name: "test"}
	_ = b.SaveFlow(ctx, doc) // version=0
	doc.Version = 0
	_ = b.SaveFlow(ctx, doc) // version=1

	// Save with correct version=1
	doc.Version = 1
	doc.Name = "correct version"
	err := b.SaveFlow(ctx, doc)
	if err != nil {
		t.Errorf("save with correct version should succeed: %v", err)
	}
	if doc.Version != 2 {
		t.Errorf("after correct update: want version 2, got %d", doc.Version)
	}
}

// --- Helpers ---

type captureNotifier struct {
	called bool
}

func (n *captureNotifier) NotifyFlowChanged(flowID string, version int) {
	n.called = true
}
