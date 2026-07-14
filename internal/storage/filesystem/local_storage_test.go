package filesystem

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/interfaces"
	testutil "pad-analyzer/internal/testutil"
)

// TestLocalStorageBackend_Contract runs the cross-backend contract suite
// against the filesystem backend. The same suite runs against Postgres in
// `database/postgres_storage_test.go::TestPostgres_Contract` so the two
// implementations cannot diverge on return-shape semantics.
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

// TestLocalStorageBackend_SaveAndLoadFlow tests saving and loading flow documents
func TestLocalStorageBackend_SaveAndLoadFlow(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	// Create test flow
	testFlow := createTestFlow("test-flow-1")

	// Save flow
	err := storage.SaveFlow(ctx, testFlow)
	testutil.AssertNoError(t, err, "Failed to save flow")

	// Load flow
	loadedFlow, err := storage.LoadFlow(ctx, "test-flow-1")
	testutil.AssertNoError(t, err, "Failed to load flow")

	// Verify flow data
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

	// Create multiple test flows owned by the test user
	flows := []*interfaces.FlowDocument{
		{ID: "flow-1", Name: "Test Flow", Description: "A test flow document", Content: []byte(`{"test": "content"}`), Metadata: interfaces.FlowMetadata{BlockCount: 10, SubflowCount: 2, MaxDepth: 3}, OwnerID: owner},
		{ID: "flow-2", Name: "Test Flow", Description: "A test flow document", Content: []byte(`{"test": "content"}`), Metadata: interfaces.FlowMetadata{BlockCount: 10, SubflowCount: 2, MaxDepth: 3}, OwnerID: owner},
		{ID: "flow-3", Name: "Test Flow", Description: "A test flow document", Content: []byte(`{"test": "content"}`), Metadata: interfaces.FlowMetadata{BlockCount: 10, SubflowCount: 2, MaxDepth: 3}, OwnerID: owner},
	}

	// Save flows
	for _, flow := range flows {
		err := storage.SaveFlow(ctx, flow)
		testutil.AssertNoError(t, err, "Failed to save flow")
	}

	// List flows owned by the test user
	listedFlows, err := storage.ListFlows(ctx, interfaces.FlowFilter{UserID: owner})
	testutil.AssertNoError(t, err, "Failed to list flows")
	testutil.AssertEqual(t, len(flows), len(listedFlows), "Flow count mismatch")

	// List with limit
	limitedFlows, err := storage.ListFlows(ctx, interfaces.FlowFilter{UserID: owner, Limit: 2})
	testutil.AssertNoError(t, err, "Failed to list flows with limit")
	testutil.AssertEqual(t, 2, len(limitedFlows), "Limited flow count mismatch")
}

// TestLocalStorageBackend_ListFlows_AllFlows locks the enumeration contract the
// migrator depends on: an unscoped filter must match nothing (defense-in-depth),
// while AllFlows must return every flow regardless of owner — including flows
// owned by different users or with no owner at all. Without this, the
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

// TestLocalStorageBackend_ListFlows_OrdersByUpdatedAtDesc is a regression guard:
// the filesystem backend previously returned directory (alphabetical) order,
// which diverged from the Postgres default (updated_at DESC) and made
// offset/limit pagination unstable (skips/dupes across pages).
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

// TestLocalStorageBackend_ListFlows_OrdersByBlocksDesc verifies the FlowSortBlocksDesc
// mode mirrors the Postgres ORDER BY (block_count DESC, updated_at DESC) rather
// than falling through to updated_at DESC (the previous divergence).
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

	// Create and save test flow
	testFlow := createTestFlow("flow-to-delete")
	err := storage.SaveFlow(ctx, testFlow)
	testutil.AssertNoError(t, err, "Failed to save flow")

	// Delete flow
	err = storage.DeleteFlow(ctx, "flow-to-delete")
	testutil.AssertNoError(t, err, "Failed to delete flow")

	// Verify flow is deleted
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

	// Create test settings
	testSettings := &interfaces.AppSettings{
		Version: 1,
		General: interfaces.GeneralSettings{
			FirstRunCompleted: true,
			LastUsedVersion:   "1.0.0",
			CheckForUpdates:   "weekly",
		},
		Appearance: interfaces.AppearanceSettings{
			Theme:   "dark",
			Density: "comfortable",
		},
	}

	// Save settings
	err := storage.SaveSettings(ctx, testSettings)
	testutil.AssertNoError(t, err, "Failed to save settings")

	// Load settings
	loadedSettings, err := storage.LoadSettings(ctx)
	testutil.AssertNoError(t, err, "Failed to load settings")

	// Verify settings
	testutil.AssertEqual(t, testSettings.Version, loadedSettings.Version, "Settings version mismatch")
	testutil.AssertEqual(t, testSettings.General.FirstRunCompleted, loadedSettings.General.FirstRunCompleted, "First run completed mismatch")
}

// TestLocalStorageBackend_SaveAndLoadConversation tests saving and loading conversations
func TestLocalStorageBackend_SaveAndLoadConversation(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	// Create test conversation
	messages := []interfaces.ChatMessage{
		{
			ID:        "msg-1",
			Role:      "user",
			Content:   "Hello",
			Timestamp: "2024-01-01T00:00:00Z",
		},
		{
			ID:        "msg-2",
			Role:      "assistant",
			Content:   "Hi there!",
			Timestamp: "2024-01-01T00:00:01Z",
		},
	}

	// Save conversation
	err := storage.SaveConversation(ctx, "flow-1", "test-scope", messages)
	testutil.AssertNoError(t, err, "Failed to save conversation")

	// Load conversation
	loadedMessages, err := storage.LoadConversation(ctx, "flow-1", "test-scope")
	testutil.AssertNoError(t, err, "Failed to load conversation")

	// Verify messages
	testutil.AssertEqual(t, len(messages), len(loadedMessages), "Message count mismatch")
	testutil.AssertEqual(t, messages[0].ID, loadedMessages[0].ID, "First message ID mismatch")
}

// TestLocalStorageBackend_Ping tests the ping method
func TestLocalStorageBackend_Ping(t *testing.T) {
	ts := testutil.NewTestSuite(t)
	defer ts.Cleanup()

	storage, _ := NewLocalStorageBackend(ts.GetTempDir())
	ctx := context.Background()

	// Ping should succeed
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

// TestLocalStorageBackend_SaveFlow_VersionConflict verifies that saving a flow
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
// goroutines using a read-modify-write-with-retry loop. flowMu must serialize
// the OCC check+write so that every increment lands exactly once and no update
// is silently lost. Run with -race to also catch data races.
func TestLocalStorageBackend_SaveFlow_ConcurrentRetry(t *testing.T) {
	storage, err := NewLocalStorageBackend(t.TempDir())
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
}

// TestLocalStorageBackend_SaveFlow_NewFlowForcesVersionZero verifies that a
// brand-new flow is always stored at version 0, matching the Postgres backend
// (INSERT ... VALUES (..., 0)). Previously the filesystem backend left
// flow.Version at whatever the caller passed, so a new flow saved with a stale
// non-zero version would later reject a legitimate Version=0 save.
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
