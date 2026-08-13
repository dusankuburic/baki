package database_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// M5: FlowSortBlocksDesc orders by metadata BlockCount descending, and the
// supporting expression index (migration v9) exists.
func TestListFlows_SortByBlocksDesc(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	suffix := time.Now().Format("150405.000000000")
	owner := "blocksort-owner-" + suffix
	type spec struct {
		id     string
		blocks int
	}
	// Deliberately out of insert order so a working sort is observable.
	flows := []spec{
		{"bs-a-" + suffix, 5},
		{"bs-b-" + suffix, 50},
		{"bs-c-" + suffix, 1},
	}
	for _, f := range flows {
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: f.id, Name: f.id, OwnerID: owner, Content: []byte("{}"),
			Metadata: interfaces.FlowMetadata{BlockCount: f.blocks},
		}); err != nil {
			t.Fatalf("SaveFlow %s: %v", f.id, err)
		}
	}
	t.Cleanup(func() {
		for _, f := range flows {
			b.DeleteFlow(ctx, f.id)
		}
	})

	// The expression index must exist so this sort mode doesn't full-scan.
	var indexExists bool
	if err := b.DB().QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = 'flows_blockcount_updated_idx')`).
		Scan(&indexExists); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if !indexExists {
		t.Error("expected flows_blockcount_updated_idx to exist (migration v9)")
	}

	list, err := b.ListFlows(ctx, interfaces.FlowFilter{
		UserID: owner, SortBy: interfaces.FlowSortBlocksDesc, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	var order []int
	for _, f := range list {
		if f.OwnerID == owner {
			order = append(order, f.Metadata.BlockCount)
		}
	}
	want := []int{50, 5, 1}
	if len(order) != len(want) {
		t.Fatalf("got %d owned flows, want %d (%v)", len(order), len(want), order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("BlocksDesc order = %v, want %v", order, want)
			break
		}
	}
}

// M4: with no blob client, SaveFlowVersion stores content in the DB column and
// records an empty blob_key; version numbers still allocate atomically and the
// content round-trips.
func TestSaveLoadFlowVersion_DBPath(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	suffix := time.Now().Format("150405.000000000")
	flowID := "fv-dbpath-" + suffix
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
		ID: flowID, Name: "fv", OwnerID: "fv-owner-" + suffix, Content: []byte("{}"),
	}); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	t.Cleanup(func() { b.DeleteFlow(ctx, flowID) })

	// Two saves → versions 1 and 2 (allocated under the lock, caller value ignored).
	for i := 1; i <= 2; i++ {
		fv := &interfaces.FlowVersion{
			ID: "ver-" + suffix + "-" + string(rune('0'+i)), FlowID: flowID, Version: 999,
			Content:   json.RawMessage(`{"snap":` + string(rune('0'+i)) + `}`),
			CreatedBy: "tester", CreatedAt: time.Now().UTC(),
		}
		if err := b.SaveFlowVersion(ctx, fv); err != nil {
			t.Fatalf("SaveFlowVersion %d: %v", i, err)
		}
		if fv.Version != i {
			t.Errorf("version = %d, want %d (server allocation)", fv.Version, i)
		}
	}

	got, err := b.LoadFlowVersion(ctx, flowID, 2)
	if err != nil {
		t.Fatalf("LoadFlowVersion(2): %v", err)
	}
	// Compare semantically — JSONB round-trips reformat whitespace.
	var parsed map[string]int
	if err := json.Unmarshal(got.Content, &parsed); err != nil {
		t.Fatalf("content not valid JSON: %s (%v)", got.Content, err)
	}
	if parsed["snap"] != 2 {
		t.Errorf("content = %s, want snap=2", got.Content)
	}

	// No blob client ⇒ blob_key must be empty (content lives in the DB column).
	var blobKey string
	if err := b.DB().QueryRowContext(ctx,
		`SELECT blob_key FROM flow_versions WHERE flow_id = $1 AND version = 2`, flowID).Scan(&blobKey); err != nil {
		t.Fatalf("read blob_key: %v", err)
	}
	if blobKey != "" {
		t.Errorf("blob_key = %q, want empty (no blob client)", blobKey)
	}
}
