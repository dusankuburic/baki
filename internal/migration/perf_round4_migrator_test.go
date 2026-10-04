package migration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// TestMigrator_RerunReadsHeadersOnly pins the migrator's read cost. The flow
// walk listed the source WITHOUT MetadataOnly, hydrating every flow's content
// on every page (the per-flow copy reloads it anyway), and the idempotency
// check loaded each flow from the DESTINATION in full — row plus content blob
// download — just to learn it exists. A rerun therefore read every flow twice
// in full to skip it. Now the listing carries no content, existence is a header
// read, and it is checked before the source load: a rerun loads nothing.
func TestMigrator_RerunReadsHeadersOnly(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()
	const n = 4
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("flow-%d", i)
		src.Flows[id] = &interfaces.FlowDocument{
			ID: id, Name: "Flow " + id, OwnerID: "u1",
			Content:   []byte(`{"subflows":[]}` + strings.Repeat(" ", 1024)),
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
	}

	res, err := migration.New(src, dst).WithBatchSize(3).Migrate(context.Background())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.FlowsMigrated != n || res.FlowsSkipped != 0 || res.FlowsFailed != 0 {
		t.Fatalf("first run: migrated/skipped/failed = %d/%d/%d, want %d/0/0", res.FlowsMigrated, res.FlowsSkipped, res.FlowsFailed, n)
	}
	if got := src.LoadFlowCalls.Load(); got != n {
		t.Fatalf("first run: source LoadFlow calls = %d, want %d (one full load per copied flow)", got, n)
	}
	if got := dst.LoadFlowCalls.Load(); got != 0 {
		t.Fatalf("first run: destination LoadFlow calls = %d, want 0 (existence is a header read)", got)
	}
	filters := src.ListFlowsFilterLog()
	if len(filters) == 0 {
		t.Fatal("the source was never listed")
	}
	for i, f := range filters {
		if !f.MetadataOnly {
			t.Fatalf("source ListFlows call %d hydrated content (MetadataOnly=false): %+v", i, f)
		}
	}
	if got := src.ListFlowsContentBytes.Load(); got != 0 {
		t.Fatalf("the listing returned %d content bytes, want 0", got)
	}

	// Rerun: every flow is already in dst. That must cost N header reads —
	// no source load (it ran before the existence check), no dst content.
	res2, err := migration.New(src, dst).WithBatchSize(3).Migrate(context.Background())
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if res2.FlowsSkipped != n || res2.FlowsMigrated != 0 || res2.FlowsFailed != 0 {
		t.Fatalf("rerun: migrated/skipped/failed = %d/%d/%d, want 0/%d/0", res2.FlowsMigrated, res2.FlowsSkipped, res2.FlowsFailed, n)
	}
	if got := dst.LoadFlowCalls.Load(); got != 0 {
		t.Fatalf("rerun: destination LoadFlow calls = %d, want 0", got)
	}
	if got := src.LoadFlowCalls.Load(); got != n {
		t.Fatalf("source LoadFlow calls across both runs = %d, want %d (not 2x: an already-migrated flow is skipped before it is loaded)", got, n)
	}
	if got := src.ListFlowsContentBytes.Load(); got != 0 {
		t.Fatalf("the listings returned %d content bytes across both runs, want 0", got)
	}
	// What was copied is still the full flow, not the metadata-only row.
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("flow-%d", i)
		if got := dst.Flows[id]; got == nil || len(got.Content) != len(src.Flows[id].Content) {
			t.Fatalf("dst %s content = %d bytes, want the source's %d", id, len(got.Content), len(src.Flows[id].Content))
		}
	}
}
