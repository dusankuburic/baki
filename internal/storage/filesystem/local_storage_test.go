package filesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/interfaces"
	testutil "pad-analyzer/internal/testutil"
)

// TestLocalStorageBackend_Contract runs the cross-backend contract suite
// against the filesystem backend (the same suite runs against Postgres), so
// the two implementations cannot diverge on return-shape semantics.
func TestLocalStorageBackend_Contract(t *testing.T) {
	b, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	contract.RunSuite(t, b)
}

// createTestFlow creates a test flow document
func createTestFlow(id string) *interfaces.FlowDocument {
	return &interfaces.FlowDocument{
		ID:          id,
		Name:        "Test Flow",
		Description: "A test flow document",
		Content:     []byte(`{"test": "content"}`),
		Metadata: interfaces.FlowMetadata{
			BlockCount:   10,
			SubflowCount: 2,
			MaxDepth:     3,
		},
	}
}

// TestNewLocalStorageBackend_SweepsCrashOrphanedTemps pins the startup sweep:
// a crash between CreateTemp and the rename strands a ".baki-tmp-*" forever
// (the old fixed "<name>.tmp" self-overwrote, so stranding was self-healing).
// Constructing the backend must remove temps older than staleTempMaxAge,
// nested ones included, while a young (live writer's) temp survives.
func TestNewLocalStorageBackend_SweepsCrashOrphanedTemps(t *testing.T) {
	dataDir := t.TempDir()

	oldTop := filepath.Join(dataDir, ".baki-tmp-stale-top")
	oldNested := filepath.Join(dataDir, "conversations", "openai", ".baki-tmp-stale-nested")
	for _, p := range []string{oldTop, oldNested} {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte("stranded"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		stale := time.Now().Add(-2 * staleTempMaxAge)
		if err := os.Chtimes(p, stale, stale); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}
	fresh := filepath.Join(dataDir, ".baki-tmp-fresh")
	if err := os.WriteFile(fresh, []byte("live writer"), 0o600); err != nil {
		t.Fatalf("write %s: %v", fresh, err)
	}

	if _, err := NewLocalStorageBackend(dataDir); err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}

	for _, p := range []string{oldTop, oldNested} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale temp %s survived the sweep (stat err = %v)", p, err)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh temp %s must survive the sweep: %v", fresh, err)
	}
}

// TestLocalStorageBackend_SaveAndLoadFlow tests saving and loading flow documents
func TestLocalStorageBackend_SaveAndLoadFlow(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	testFlow := createTestFlow("test-flow-1")

	err := storage.SaveFlow(ctx, testFlow)
	testutil.AssertNoError(t, err, "Failed to save flow")

	loadedFlow, err := storage.LoadFlow(ctx, "test-flow-1")
	testutil.AssertNoError(t, err, "Failed to load flow")

	testutil.AssertEqual(t, testFlow.ID, loadedFlow.ID, "Flow ID mismatch")
	testutil.AssertEqual(t, testFlow.Name, loadedFlow.Name, "Flow name mismatch")
	testutil.AssertEqual(t, testFlow.Description, loadedFlow.Description, "Flow description mismatch")
}

// TestLocalStorageBackend_ListFlows tests listing flow documents
func TestLocalStorageBackend_ListFlows(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	owner := "test-user"

	flows := []*interfaces.FlowDocument{
		{ID: "flow-1", Name: "Test Flow", Description: "A test flow document", Content: []byte(`{"test": "content"}`), Metadata: interfaces.FlowMetadata{BlockCount: 10, SubflowCount: 2, MaxDepth: 3}, OwnerID: owner},
		{ID: "flow-2", Name: "Test Flow", Description: "A test flow document", Content: []byte(`{"test": "content"}`), Metadata: interfaces.FlowMetadata{BlockCount: 10, SubflowCount: 2, MaxDepth: 3}, OwnerID: owner},
		{ID: "flow-3", Name: "Test Flow", Description: "A test flow document", Content: []byte(`{"test": "content"}`), Metadata: interfaces.FlowMetadata{BlockCount: 10, SubflowCount: 2, MaxDepth: 3}, OwnerID: owner},
	}

	for _, flow := range flows {
		err := storage.SaveFlow(ctx, flow)
		testutil.AssertNoError(t, err, "Failed to save flow")
	}

	listedFlows, err := storage.ListFlows(ctx, interfaces.FlowFilter{UserID: owner})
	testutil.AssertNoError(t, err, "Failed to list flows")
	testutil.AssertEqual(t, len(flows), len(listedFlows), "Flow count mismatch")

	limitedFlows, err := storage.ListFlows(ctx, interfaces.FlowFilter{UserID: owner, Limit: 2})
	testutil.AssertNoError(t, err, "Failed to list flows with limit")
	testutil.AssertEqual(t, 2, len(limitedFlows), "Limited flow count mismatch")
}

// TestLocalStorageBackend_ListFlows_AllFlows locks the enumeration contract the
// migrator depends on: an unscoped filter matches nothing (defense-in-depth),
// while AllFlows returns every flow regardless of owner — otherwise the
// filesystem→cloud migration silently copies zero flows.
func TestLocalStorageBackend_ListFlows_AllFlows(t *testing.T) {
	storage, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	ctx := context.Background()

	flows := []*interfaces.FlowDocument{
		{ID: "flow-a", Name: "A", Content: []byte(`{}`), OwnerID: "user-1"},
		{ID: "flow-b", Name: "B", Content: []byte(`{}`), OwnerID: "user-2"},
		{ID: "flow-c", Name: "C", Content: []byte(`{}`), OwnerID: ""},
	}
	for _, f := range flows {
		if err := storage.SaveFlow(ctx, f); err != nil {
			t.Fatalf("SaveFlow %s: %v", f.ID, err)
		}
	}

	// Unscoped filter must match nothing — the guard the migrator would
	// accidentally trip if it didn't set AllFlows.
	none, err := storage.ListFlows(ctx, interfaces.FlowFilter{})
	testutil.AssertNoError(t, err, "ListFlows empty filter")
	testutil.AssertEqual(t, 0, len(none), "empty filter must match no flows")

	// AllFlows must enumerate every flow regardless of owner.
	all, err := storage.ListFlows(ctx, interfaces.FlowFilter{AllFlows: true})
	testutil.AssertNoError(t, err, "ListFlows AllFlows")
	testutil.AssertEqual(t, len(flows), len(all), "AllFlows must return every flow")
}

// TestLocalStorageBackend_ListFlows_OrdersByUpdatedAtDesc is a regression
// guard: the backend previously returned directory order, diverging from the
// Postgres updated_at DESC and making offset/limit pagination unstable.
func TestLocalStorageBackend_ListFlows_OrdersByUpdatedAtDesc(t *testing.T) {
	storage, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	ctx := context.Background()
	owner := "u"

	// Save in a fixed order with distinct UpdatedAt timestamps.
	mk := func(id string, updatedAt time.Time) {
		f := &interfaces.FlowDocument{ID: id, Name: id, Content: []byte(`{}`), OwnerID: owner}
		f.UpdatedAt = updatedAt
		if err := storage.SaveFlow(ctx, f); err != nil {
			t.Fatalf("SaveFlow %s: %v", id, err)
		}
	}
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mk("alpha", base.Add(1*time.Hour))   // oldest
	mk("bravo", base.Add(3*time.Hour))   // newest
	mk("charlie", base.Add(2*time.Hour)) // middle

	got, err := storage.ListFlows(ctx, interfaces.FlowFilter{UserID: owner})
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 flows, got %d", len(got))
	}
	// Expected descending by UpdatedAt: bravo, charlie, alpha.
	want := []string{"bravo", "charlie", "alpha"}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("position %d: got %s, want %s (not ordered by updated_at DESC)", i, got[i].ID, w)
		}
	}

	// Pagination must be stable under the new ordering: offset 1, limit 1 → charlie.
	page, err := storage.ListFlows(ctx, interfaces.FlowFilter{UserID: owner, Offset: 1, Limit: 1})
	if err != nil {
		t.Fatalf("ListFlows paged: %v", err)
	}
	if len(page) != 1 || page[0].ID != "charlie" {
		t.Errorf("offset/limit page = %+v, want [charlie]", page)
	}
}

// TestLocalStorageBackend_ListFlows_OrdersByBlocksDesc verifies the
// FlowSortBlocksDesc mode mirrors the Postgres ORDER BY (block_count DESC,
// updated_at DESC) instead of falling through to updated_at DESC.
func TestLocalStorageBackend_ListFlows_OrdersByBlocksDesc(t *testing.T) {
	storage, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	ctx := context.Background()
	owner := "u"
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	mk := func(id string, blockCount int, updatedAt time.Time) {
		f := &interfaces.FlowDocument{
			ID: id, Name: id, Content: []byte(`{}`), OwnerID: owner,
			Metadata: interfaces.FlowMetadata{BlockCount: blockCount},
		}
		f.UpdatedAt = updatedAt
		if err := storage.SaveFlow(ctx, f); err != nil {
			t.Fatalf("SaveFlow %s: %v", id, err)
		}
	}
	// "big" has 100 blocks but oldest timestamp; "small" has 1 block but newest.
	// Under BlocksDesc, "big" must rank first despite being oldest.
	mk("big", 100, base.Add(1*time.Hour))
	mk("mid", 50, base.Add(2*time.Hour))
	mk("small", 1, base.Add(3*time.Hour))

	got, err := storage.ListFlows(ctx, interfaces.FlowFilter{UserID: owner, SortBy: interfaces.FlowSortBlocksDesc})
	if err != nil {
		t.Fatalf("ListFlows BlocksDesc: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 flows, got %d", len(got))
	}
	want := []string{"big", "mid", "small"} // by block count, not recency
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("BlocksDesc position %d: got %s, want %s", i, got[i].ID, w)
		}
	}
}

// TestLocalStorageBackend_DeleteFlow tests deleting flow documents
func TestLocalStorageBackend_DeleteFlow(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	testFlow := createTestFlow("flow-to-delete")
	err := storage.SaveFlow(ctx, testFlow)
	testutil.AssertNoError(t, err, "Failed to save flow")

	err = storage.DeleteFlow(ctx, "flow-to-delete")
	testutil.AssertNoError(t, err, "Failed to delete flow")

	_, err = storage.LoadFlow(ctx, "flow-to-delete")
	if err == nil {
		t.Fatal("Expected error when loading deleted flow")
	}
}

// TestLocalStorageBackend_SaveAndLoadSettings tests saving and loading settings
func TestLocalStorageBackend_SaveAndLoadSettings(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	testSettings := &interfaces.AppSettings{
		Version: 1,
		General: interfaces.GeneralSettings{
			FirstRunCompleted: true,
		},
		Appearance: interfaces.AppearanceSettings{
			Theme:   "dark",
			Density: "comfortable",
		},
	}

	err := storage.SaveSettings(ctx, testSettings)
	testutil.AssertNoError(t, err, "Failed to save settings")

	loadedSettings, err := storage.LoadSettings(ctx)
	testutil.AssertNoError(t, err, "Failed to load settings")

	testutil.AssertEqual(t, testSettings.Version, loadedSettings.Version, "Settings version mismatch")
	testutil.AssertEqual(t, testSettings.General.FirstRunCompleted, loadedSettings.General.FirstRunCompleted, "First run completed mismatch")
}

// TestLocalStorageBackend_Ping tests the ping method
func TestLocalStorageBackend_Ping(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	err := storage.Ping(ctx)
	testutil.AssertNoError(t, err, "Ping failed")
}

// TestLocalStorageBackend_SaveFlow_OCCIncrementsVersion verifies that saving an
// existing flow at its current version bumps the stored version by one.
func TestLocalStorageBackend_SaveFlow_OCCIncrementsVersion(t *testing.T) {
	storage, err := NewLocalStorageBackend(t.TempDir())
	testutil.AssertNoError(t, err, "NewLocalStorageBackend")
	ctx := context.Background()

	flow := createTestFlow("occ-flow")
	testutil.AssertNoError(t, storage.SaveFlow(ctx, flow), "first save")

	loaded, err := storage.LoadFlow(ctx, "occ-flow")
	testutil.AssertNoError(t, err, "load after first save")
	testutil.AssertEqual(t, 0, loaded.Version, "initial version should be 0")

	// Save again at the loaded version: OCC should accept and bump to 1.
	loaded.Name = "Updated"
	testutil.AssertNoError(t, storage.SaveFlow(ctx, loaded), "second save")

	reloaded, err := storage.LoadFlow(ctx, "occ-flow")
	testutil.AssertNoError(t, err, "load after second save")
	testutil.AssertEqual(t, 1, reloaded.Version, "version should bump to 1")
	testutil.AssertEqual(t, "Updated", reloaded.Name, "name should be updated")
}

// TestLocalStorageBackend_SaveFlow_VersionConflict verifies that saving
// against a stale version is rejected with ErrVersionConflict and leaves the
// stored flow untouched.
func TestLocalStorageBackend_SaveFlow_VersionConflict(t *testing.T) {
	storage, err := NewLocalStorageBackend(t.TempDir())
	testutil.AssertNoError(t, err, "NewLocalStorageBackend")
	ctx := context.Background()

	flow := createTestFlow("conflict-flow")
	testutil.AssertNoError(t, storage.SaveFlow(ctx, flow), "first save")

	// Two readers grab the same version-0 snapshot.
	readerA, err := storage.LoadFlow(ctx, "conflict-flow")
	testutil.AssertNoError(t, err, "load reader A")
	readerB, err := storage.LoadFlow(ctx, "conflict-flow")
	testutil.AssertNoError(t, err, "load reader B")

	// A writes first and wins (version -> 1).
	readerA.Name = "A wins"
	testutil.AssertNoError(t, storage.SaveFlow(ctx, readerA), "reader A save")

	// B writes against the now-stale version 0 and must be rejected.
	readerB.Name = "B loses"
	err = storage.SaveFlow(ctx, readerB)
	if !errors.Is(err, interfaces.ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict, got %v", err)
	}

	// The losing write must not have been applied.
	final, err := storage.LoadFlow(ctx, "conflict-flow")
	testutil.AssertNoError(t, err, "load final")
	testutil.AssertEqual(t, "A wins", final.Name, "stored name should be A's")
	testutil.AssertEqual(t, 1, final.Version, "version should be 1")
}

// TestLocalStorageBackend_SaveFlow_ConcurrentRetry hammers SaveFlow from many
// goroutines with read-modify-write retry: flowMu must serialize the OCC
// check+write so every increment lands exactly once. Run with -race.
func TestLocalStorageBackend_SaveFlow_ConcurrentRetry(t *testing.T) {
	dataDir := t.TempDir()
	storage, err := NewLocalStorageBackend(dataDir)
	testutil.AssertNoError(t, err, "NewLocalStorageBackend")
	ctx := context.Background()

	testutil.AssertNoError(t, storage.SaveFlow(ctx, createTestFlow("hot-flow")), "seed save")

	const writers = 16
	var wg sync.WaitGroup
	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			for {
				cur, err := storage.LoadFlow(ctx, "hot-flow")
				if err != nil {
					t.Errorf("load: %v", err)
					return
				}
				if err := storage.SaveFlow(ctx, cur); err == nil {
					return // committed our increment
				} else if !errors.Is(err, interfaces.ErrVersionConflict) {
					t.Errorf("unexpected save error: %v", err)
					return
				}
				// Conflict: another writer won the race; retry with fresh state.
			}
		}()
	}
	wg.Wait()

	final, err := storage.LoadFlow(ctx, "hot-flow")
	testutil.AssertNoError(t, err, "load final")
	// Seed left version 0; each of the writers commits exactly one increment.
	testutil.AssertEqual(t, writers, final.Version, "every writer's increment should land exactly once")

	assertNoTempStragglers(t, dataDir)
}

// assertNoTempStragglers fails if any atomicWrite temp survived under root —
// a leftover means a cleanup path was missed.
func assertNoTempStragglers(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasPrefix(d.Name(), ".baki-tmp-") || strings.HasSuffix(d.Name(), ".tmp")) {
			t.Errorf("leftover temp file: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// TestLocalStorageBackend_SaveSettings_ConcurrentWriters locks the temp-file
// isolation of atomicWrite: settings.json is written by three unsynchronized
// Save*Settings methods, and a shared fixed temp name let two O_TRUNC writers
// interleave and publish a mixed body. Each call must build its body in its
// OWN temp so the last rename is the only deciding event. Run with -race.
func TestLocalStorageBackend_SaveSettings_ConcurrentWriters(t *testing.T) {
	dataDir := t.TempDir()
	storage, err := NewLocalStorageBackend(dataDir)
	testutil.AssertNoError(t, err, "NewLocalStorageBackend")
	ctx := context.Background()

	const writers = 8
	const rounds = 25
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := range writers {
		go func() {
			defer wg.Done()
			for range rounds {
				s := &interfaces.AppSettings{
					Version: 1,
					AI:      interfaces.AISettings{ActiveProvider: fmt.Sprintf("writer-%d", w)},
				}
				if err := storage.SaveSettings(ctx, s); err != nil {
					t.Errorf("save: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// LoadSettings re-parses the file, so an interleaved/truncated body fails
	// here rather than at the next consumer.
	final, err := storage.LoadSettings(ctx)
	testutil.AssertNoError(t, err, "load settings after concurrent writes")

	matched := false
	for w := range writers {
		if final.AI.ActiveProvider == fmt.Sprintf("writer-%d", w) {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("final settings are not exactly one writer's shape: activeProvider=%q", final.AI.ActiveProvider)
	}
	testutil.AssertEqual(t, 1, final.Version, "version should be the one written shape's")

	assertNoTempStragglers(t, dataDir)
}

// TestLocalStorageBackend_SaveFlow_NewFlowForcesVersionZero verifies a
// brand-new flow is stored at version 0, matching Postgres — the backend
// previously kept whatever the caller passed, so a stale non-zero version
// later rejected a legitimate Version=0 save.
func TestLocalStorageBackend_SaveFlow_NewFlowForcesVersionZero(t *testing.T) {
	storage, err := NewLocalStorageBackend(t.TempDir())
	testutil.AssertNoError(t, err, "NewLocalStorageBackend")
	ctx := context.Background()

	// A new flow carrying a fabricated, non-zero version (e.g. a doc struct
	// reused from a prior save).
	flow := createTestFlow("versioned-flow")
	flow.Version = 7
	testutil.AssertNoError(t, storage.SaveFlow(ctx, flow), "save new flow")

	loaded, err := storage.LoadFlow(ctx, "versioned-flow")
	testutil.AssertNoError(t, err, "load new flow")
	testutil.AssertEqual(t, 0, loaded.Version, "new flow must start at version 0 (match Postgres)")

	// A subsequent legitimate save at version 0 must succeed (no spurious conflict).
	loaded.Name = "v2"
	testutil.AssertNoError(t, storage.SaveFlow(ctx, loaded), "second save at version 0")
	reloaded, err := storage.LoadFlow(ctx, "versioned-flow")
	testutil.AssertNoError(t, err, "load after second save")
	testutil.AssertEqual(t, 1, reloaded.Version, "version should bump to 1")
}
