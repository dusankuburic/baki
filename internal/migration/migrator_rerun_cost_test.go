package migration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// countingSource counts the source's full flow loads.
type countingSource struct {
	migration.LegacySource
	loads atomic.Int64
}

func (s *countingSource) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	s.loads.Add(1)
	return s.LegacySource.LoadFlow(ctx, id)
}

// TestMigrator_RerunReadsHeadersOnly pins the migrator's read cost. The flow
// walk used to hydrate every flow's content on every page (the per-flow copy
// reloads it anyway), and the idempotency check loaded each flow from the
// DESTINATION in full — row plus content blob download — just to learn it
// exists. A rerun therefore read every flow twice in full to skip it. Now the
// listing carries IDs only, existence is a header read, and it is checked
// before the source load: a rerun loads nothing.
func TestMigrator_RerunReadsHeadersOnly(t *testing.T) {
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()
	const n = 4
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("flow-%d", i)
		d.Flow(&interfaces.FlowDocument{
			ID: id, Name: "Flow " + id, OwnerID: "u1",
			Content:   json.RawMessage(`{"subflows":[],"pad":"` + strings.Repeat("x", 1024) + `"}`),
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		})
	}
	src := &countingSource{LegacySource: openSource(t, d)}

	res, err := migration.New(src, dst).WithBatchSize(3).Migrate(context.Background())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.FlowsMigrated != n || res.FlowsSkipped != 0 || res.FlowsFailed != 0 {
		t.Fatalf("first run: migrated/skipped/failed = %d/%d/%d, want %d/0/0", res.FlowsMigrated, res.FlowsSkipped, res.FlowsFailed, n)
	}
	if got := src.loads.Load(); got != n {
		t.Fatalf("first run: source LoadFlow calls = %d, want %d (one full load per copied flow)", got, n)
	}
	if got := dst.LoadFlowCalls.Load(); got != 0 {
		t.Fatalf("first run: destination LoadFlow calls = %d, want 0 (existence is a header read)", got)
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
	if got := src.loads.Load(); got != n {
		t.Fatalf("source LoadFlow calls across both runs = %d, want %d (not 2x: an already-migrated flow is skipped before it is loaded)", got, n)
	}
	// What was copied is the full flow.
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("flow-%d", i)
		want, err := src.LegacySource.LoadFlow(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got := dst.Flows[id]; got == nil || len(got.Content) < 1024 || !bytes.Equal(got.Content, want.Content) {
			t.Fatalf("dst %s content differs from the source's %d bytes", id, len(want.Content))
		}
	}
}
