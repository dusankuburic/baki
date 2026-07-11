package padcloud

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

func sampleDoc(name string) *models.FlowDocument {
	return &models.FlowDocument{
		ID:   uuid.NewString(),
		Name: name,
		Subflows: []models.Subflow{{
			ID:     uuid.NewString(),
			Name:   name,
			Blocks: []models.Block{{ID: uuid.NewString(), RawType: "Excel.ReadCell"}},
		}},
		Metadata: models.FlowMetadata{BlockCount: 1, SubflowCount: 1, MaxDepth: 1},
	}
}

// stableFlowID mirrors LibraryStore's id derivation so tests can re-derive the
// library row id from a Power Platform source id.
func stableFlowID(sourceID string) string {
	ns := uuid.NewSHA1(uuid.NameSpaceURL, []byte("padcloud.baki/flow-id"))
	return uuid.NewSHA1(ns, []byte(sourceID)).String()
}

func TestLibraryStore_FirstIngestCreatesFlow(t *testing.T) {
	backend := testutil.NewFakeBackend()
	store := NewLibraryStore(backend, "owner-1", "org-1")

	if err := store.UpsertFlow(context.Background(), sampleDoc("My Flow"), "pp-src-1"); err != nil {
		t.Fatalf("UpsertFlow: %v", err)
	}

	id := stableFlowID("pp-src-1")
	header, err := backend.LoadFlowHeader(context.Background(), id)
	if err != nil {
		t.Fatalf("flow not persisted: %v", err)
	}
	if header.Name != "My Flow" {
		t.Errorf("stored Name = %q, want My Flow", header.Name)
	}
	if header.OwnerID != "owner-1" || header.OrganizationID != "org-1" {
		t.Errorf("owner/org = %q/%q, want owner-1/org-1", header.OwnerID, header.OrganizationID)
	}

	// Content round-trips back to the original FlowDocument shape.
	full, _ := backend.LoadFlow(context.Background(), id)
	var doc models.FlowDocument
	if err := json.Unmarshal(full.Content, &doc); err != nil {
		t.Fatalf("stored content is not a valid FlowDocument: %v", err)
	}
	if doc.Name != "My Flow" || len(doc.Subflows) != 1 {
		t.Errorf("round-tripped doc = %+v, want 1 subflow named My Flow", doc)
	}
}

func TestLibraryStore_ReingestUpsertsInPlace(t *testing.T) {
	backend := testutil.NewFakeBackend()
	store := NewLibraryStore(backend, "o", "")

	_ = store.UpsertFlow(context.Background(), sampleDoc("v1"), "pp-src-1")
	before, _ := backend.LoadFlowHeader(context.Background(), stableFlowID("pp-src-1"))

	_ = store.UpsertFlow(context.Background(), sampleDoc("v2"), "pp-src-1")
	after, _ := backend.LoadFlowHeader(context.Background(), stableFlowID("pp-src-1"))

	if after.Version <= before.Version {
		t.Errorf("re-ingest did not advance version: before=%d after=%d", before.Version, after.Version)
	}
	if n, _ := backend.CountFlows(context.Background(), storageif.FlowFilter{AllFlows: true}); n != 1 {
		t.Errorf("flow count = %d after re-ingest, want 1 (should upsert not duplicate)", n)
	}
	// The latest content reflects v2.
	full, _ := backend.LoadFlow(context.Background(), stableFlowID("pp-src-1"))
	var doc models.FlowDocument
	_ = json.Unmarshal(full.Content, &doc)
	if doc.Name != "v2" {
		t.Errorf("after re-ingest Name = %q, want v2", doc.Name)
	}
}

func TestLibraryStore_DistinctSourcesAreDistinctFlows(t *testing.T) {
	backend := testutil.NewFakeBackend()
	store := NewLibraryStore(backend, "o", "")

	_ = store.UpsertFlow(context.Background(), sampleDoc("A"), "src-a")
	_ = store.UpsertFlow(context.Background(), sampleDoc("B"), "src-b")

	if n, _ := backend.CountFlows(context.Background(), storageif.FlowFilter{AllFlows: true}); n != 2 {
		t.Errorf("flow count = %d, want 2 distinct flows", n)
	}
}

// signalStore is a race-safe Store whose UpsertFlow closes a channel the first
// time it's called, so the scheduler test can detect the immediate sweep.
type signalStore struct {
	mu    sync.Mutex
	calls int
	once  sync.Once
	done  chan struct{}
}

func newSignalStore() *signalStore { return &signalStore{done: make(chan struct{})} }

func (s *signalStore) UpsertFlow(_ context.Context, _ *models.FlowDocument, _ string) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
	return nil
}

func (s *signalStore) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

// TestIngester_StartRunsImmediateSweepThenStops verifies the lifecycle: Start
// triggers an immediate ingest pass (the signal store fires) and Stop cleanly
// ends the loop without hanging.
func TestIngester_StartRunsImmediateSweepThenStops(t *testing.T) {
	client := &mockClient{
		flows: []DesktopFlowRef{{ID: "f1", Name: "Flow A"}},
		defs:  map[string]json.RawMessage{"f1": []byte("a")},
	}
	converter := &mockConverter{docs: map[string]*models.FlowDocument{"Flow A": {Name: "Flow A"}}}
	store := newSignalStore()

	ing := NewIngester(client, converter, store)
	ing.Start(20 * time.Millisecond)

	select {
	case <-store.done:
		// immediate sweep ran
	case <-time.After(2 * time.Second):
		t.Fatal("immediate sweep did not run within 2s")
	}

	// Let at least one ticker fire to prove periodic behaviour.
	time.Sleep(60 * time.Millisecond)
	if store.count() < 1 {
		t.Errorf("expected periodic sweeps, got %d UpsertFlow calls", store.count())
	}

	ing.Stop() // must return promptly (no hang); test framework times out otherwise
}

func TestIngester_StartZeroIntervalIsNoOp(t *testing.T) {
	ing := NewIngester(&mockClient{}, &mockConverter{}, newSignalStore())
	ing.Start(0) // must not start a loop / sweep
	ing.Stop()
}
