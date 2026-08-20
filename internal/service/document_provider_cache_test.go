package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// loadCountingBackend counts content loads so tests can prove cache hits
// (header loads still happen per resolve — they're the validity check).
type loadCountingBackend struct {
	*testutil.FakeBackend
	loadFlowCalls       int
	loadFlowHeaderCalls int
}

func (b *loadCountingBackend) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	b.loadFlowCalls++
	return b.FakeBackend.LoadFlow(ctx, id)
}

func (b *loadCountingBackend) LoadFlowHeader(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	b.loadFlowHeaderCalls++
	return b.FakeBackend.LoadFlowHeader(ctx, id)
}

func seedCloudFlow(t *testing.T, backend *testutil.FakeBackend, id string) {
	t.Helper()
	doc := &models.FlowDocument{
		ID:   id,
		Name: "Flow " + id,
		Subflows: []models.Subflow{{
			ID: id + "-sf", Name: "Main",
			Blocks: []models.Block{{
				ID: id + "-b1", Name: "Block", Type: models.BlockTypeAction,
				RawType: "Display.ShowMessage", Children: []models.Block{},
			}},
		}},
	}
	content, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	if err := backend.SaveFlow(context.Background(), &interfaces.FlowDocument{
		ID: id, Name: doc.Name, Content: content, OwnerID: "u1", Version: 0,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestCloudDocumentProvider_CachesByUnchangedVersion(t *testing.T) {
	backend := &loadCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlow(t, backend.FakeBackend, "f1")
	p := NewCloudDocumentProvider(backend)
	ctx := context.Background()

	doc1, err := p.ResolveDoc(ctx, "f1")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	for i := 0; i < 5; i++ {
		doc2, err := p.ResolveDoc(ctx, "f1")
		if err != nil {
			t.Fatalf("warm resolve %d: %v", i, err)
		}
		if doc2 != doc1 {
			t.Error("warm resolve should return the cached doc pointer")
		}
	}
	if backend.loadFlowCalls != 1 {
		t.Errorf("expected exactly 1 content load (cache should serve the rest), got %d", backend.loadFlowCalls)
	}
	if backend.loadFlowHeaderCalls != 6 {
		t.Errorf("expected a header check per resolve (6), got %d", backend.loadFlowHeaderCalls)
	}
}

func TestCloudDocumentProvider_ReloadsAfterVersionBump(t *testing.T) {
	backend := &loadCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlow(t, backend.FakeBackend, "f1")
	p := NewCloudDocumentProvider(backend)
	ctx := context.Background()

	if _, err := p.ResolveDoc(ctx, "f1"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Simulate any writer (same process, another replica, ingester): a save
	// bumps the OCC version. FakeBackend's SaveFlow does version+1 and the
	// caller passes the current version to satisfy OCC. The doc's identity
	// comes from the stored Content JSON, so edit that (as a real save would).
	cur, err := backend.LoadFlow(ctx, "f1")
	if err != nil {
		t.Fatalf("load current: %v", err)
	}
	cur.Name = "Flow f1 (edited)"
	cur.Content = []byte(`{"id":"f1","name":"Flow f1 (edited)","subflows":[{"id":"f1-sf","name":"Main","blocks":[{"id":"f1-b1","name":"Block","type":"action","rawType":"Display.ShowMessage","children":[]}]}]}`)
	if err := backend.SaveFlow(ctx, cur); err != nil {
		t.Fatalf("edit save: %v", err)
	}

	doc, err := p.ResolveDoc(ctx, "f1")
	if err != nil {
		t.Fatalf("resolve after edit: %v", err)
	}
	if doc.Name != "Flow f1 (edited)" {
		t.Errorf("stale doc served after version bump: name=%q", doc.Name)
	}
	// loadFlowCalls: initial resolve + the test's own LoadFlow + the reload.
	if backend.loadFlowCalls != 3 {
		t.Errorf("expected reload after version bump (3 loads incl. test's own), got %d", backend.loadFlowCalls)
	}
}

func TestCloudDocumentProvider_DeletedFlowPropagatesNotFound(t *testing.T) {
	backend := &loadCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlow(t, backend.FakeBackend, "f1")
	p := NewCloudDocumentProvider(backend)
	ctx := context.Background()

	if _, err := p.ResolveDoc(ctx, "f1"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	backend.FakeBackend.Flows["f1"].Version = 99 // simulate out-of-band delete+recreate
	if _, err := p.ResolveDoc(ctx, "gone"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown flow, got %v", err)
	}
}

func TestCloudDocumentProvider_InvalidateForcesReload(t *testing.T) {
	backend := &loadCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlow(t, backend.FakeBackend, "f1")
	p := NewCloudDocumentProvider(backend)
	ctx := context.Background()

	if _, err := p.ResolveDoc(ctx, "f1"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	p.Invalidate("f1")
	if _, err := p.ResolveDoc(ctx, "f1"); err != nil {
		t.Fatalf("resolve after invalidate: %v", err)
	}
	if backend.loadFlowCalls != 2 {
		t.Errorf("expected reload after explicit invalidation, got %d loads", backend.loadFlowCalls)
	}
}

// BenchmarkResolveDocWarm measures the CACHED resolve path: one indexed header
// query + a map hit, no blob download / JSON unmarshal / index rebuild. This
// is the per-request cost paid by every flow-scoped endpoint on the hot flow,
// so a regression here (e.g. accidental cache bypass) taxes search-as-you-type,
// diff, exports, and every chat turn.
func BenchmarkResolveDocWarm(b *testing.B) {
	backend := &loadCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlow(&testing.T{}, backend.FakeBackend, "f1")
	p := NewCloudDocumentProvider(backend)
	ctx := context.Background()
	if _, err := p.ResolveDoc(ctx, "f1"); err != nil {
		b.Fatalf("warm-up resolve: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.ResolveDoc(ctx, "f1"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResolveDocCold measures the uncached path (blob-equivalent load +
// unmarshal + index rebuild) for comparison — the cost the cache removes.
func BenchmarkResolveDocCold(b *testing.B) {
	backend := &loadCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlow(&testing.T{}, backend.FakeBackend, "f1")
	p := NewCloudDocumentProvider(backend)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Invalidate("f1")
		if _, err := p.ResolveDoc(ctx, "f1"); err != nil {
			b.Fatal(err)
		}
	}
}
