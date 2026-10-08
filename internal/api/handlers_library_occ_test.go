package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

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

// flowViewersProbe records the flow.changed notifications the realtime subscriber sends.
type flowViewersProbe struct {
	mu   sync.Mutex
	sent map[string][]int
}

func (p *flowViewersProbe) NotifyFlowChanged(flowID string, version int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent[flowID] = append(p.sent[flowID], version)
}

// TestLibraryUpdate_NotifiesTheFlowsViewers: a library update reaches the flow's open viewers through the
// FlowChanged event and the realtime subscriber (as di.SubscribeRealtime wires it), with a stamp no viewer can have
// applied (never the stored version); a rejected update notifies no one.
func TestLibraryUpdate_NotifiesTheFlowsViewers(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("nflow", "alice")
	probe := &flowViewersProbe{sent: map[string][]int{}}
	eventbus.Subscribe(busOf(t, rt), eventbus.Async, "ws-flow-changed", service.NotifyFlowViewers(probe))

	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/library/nflow", bearer, map[string]any{"name": "notified"})
	checkStatus(t, rr, http.StatusOK)
	var body struct {
		Version int `json:"version"`
	}
	decodeJSON(t, rr, &body)
	if got := probe.sent["nflow"]; len(got) != 1 || got[0] == body.Version {
		t.Errorf("viewers were sent %v, want one notification whose version is not the stored %d", got, body.Version)
	}

	other := jwtBearer(t, rt, "mallory", "mallory@example.com")
	rr = doRequestWithAuth(t, rt, http.MethodPut, "/api/library/nflow", other, map[string]any{"name": "nope"})
	if rr.Code == http.StatusOK {
		t.Fatalf("an outsider updated the flow: %d", rr.Code)
	}
	if got := probe.sent["nflow"]; len(got) != 1 {
		t.Errorf("a rejected update notified the viewers: %v", got)
	}
}

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

	stale := &storageif.FlowDocument{ID: "f3", Name: "stale", Version: 99}
	err := b.SaveFlow(ctx, stale)
	if err == nil {
		t.Fatal("expected ErrVersionConflict for stale version 99, got nil")
	}
}

// Version 0 is an expected version like any other (Postgres's WHERE version = $n): once the flow is at 1, a save
// expecting 0 conflicts instead of overwriting.
func TestFakeBackend_OCC_VersionZeroIsChecked(t *testing.T) {
	b := testutil.NewFakeBackend()
	ctx := context.Background()

	doc := &storageif.FlowDocument{ID: "f4", Name: "test"}
	_ = b.SaveFlow(ctx, doc) // version=0
	doc.Version = 0
	_ = b.SaveFlow(ctx, doc) // version=1
	doc.Version = 0
	doc.Name = "force overwrite"
	if err := b.SaveFlow(ctx, doc); !errors.Is(err, storageif.ErrVersionConflict) {
		t.Errorf("stale version 0: got %v, want ErrVersionConflict", err)
	}
	if got, _ := b.LoadFlow(ctx, "f4"); got.Name != "test" || got.Version != 1 {
		t.Errorf("after the rejected save: %q at version %d, want \"test\" at 1", got.Name, got.Version)
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
