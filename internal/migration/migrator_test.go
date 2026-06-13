package migration_test

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// --- tests ---

func TestMigrator_MigratesFlows(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	for i := 0; i < 3; i++ {
		id := string(rune('a' + i))
		src.Flows[id] = &interfaces.FlowDocument{
			ID:        id,
			Name:      "Flow " + id,
			Content:   []byte(`{"subflows":[]}`),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
	}

	res, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 3 {
		t.Errorf("FlowsMigrated: want 3, got %d", res.FlowsMigrated)
	}
	if res.FlowsFailed != 0 {
		t.Errorf("FlowsFailed: want 0, got %d", res.FlowsFailed)
	}
	if !res.SettingsMoved {
		t.Error("SettingsMoved should be true")
	}
	if len(dst.Flows) != 3 {
		t.Errorf("dst flows: want 3, got %d", len(dst.Flows))
	}
}

func TestMigrator_SkipsAlreadyMigrated(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	flow := &interfaces.FlowDocument{
		ID: "existing", Name: "Existing",
		Content: []byte("{}"),
	}
	src.Flows["existing"] = flow
	dst.Flows["existing"] = flow // already there

	res, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 0 {
		t.Errorf("expected 0 migrated (already present), got %d", res.FlowsMigrated)
	}
}

func TestMigrator_InvalidFlowRecordedAsFailure(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	src.Flows["bad"] = &interfaces.FlowDocument{
		ID:      "bad",
		Name:    "", // invalid: no name
		Content: []byte("not-json"),
	}

	res, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsFailed != 1 {
		t.Errorf("FlowsFailed: want 1, got %d", res.FlowsFailed)
	}
	if len(res.Errors) != 1 {
		t.Errorf("expected 1 error entry, got %d", len(res.Errors))
	}
}

// TestMigrator_PartialFailure_RerunCompletes locks in the documented
// re-run model: when a first attempt fails some flows because the
// destination wasn't ready for them, fixing the underlying issue and
// re-running the migrator must complete the partial state without
// re-doing the already-migrated rows.
func TestMigrator_PartialFailure_RerunCompletes(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	// Seed three flows; mark one as "bad" by giving it a malformed name
	// the validator rejects.
	src.Flows["ok-1"] = &interfaces.FlowDocument{
		ID: "ok-1", Name: "Good Flow 1", Content: []byte(`{}`),
	}
	src.Flows["bad"] = &interfaces.FlowDocument{
		ID: "bad", Name: "", Content: []byte("not-json"),
	}
	src.Flows["ok-2"] = &interfaces.FlowDocument{
		ID: "ok-2", Name: "Good Flow 2", Content: []byte(`{}`),
	}

	// First run: 2 succeed, 1 fails validation.
	res, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.FlowsMigrated != 2 {
		t.Errorf("first run: migrated %d, want 2", res.FlowsMigrated)
	}
	if res.FlowsFailed != 1 {
		t.Errorf("first run: failed %d, want 1", res.FlowsFailed)
	}

	// Operator fixes the bad flow at the source and re-runs.
	src.Flows["bad"].Name = "Fixed Name"
	src.Flows["bad"].Content = []byte(`{}`)

	res2, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	// The previously-migrated flows must be skipped (idempotent), and the
	// previously-failed flow must now succeed.
	if res2.FlowsMigrated != 1 {
		t.Errorf("rerun: migrated %d, want 1 (only the fixed flow)", res2.FlowsMigrated)
	}
	if res2.FlowsSkipped != 2 {
		t.Errorf("rerun: skipped %d, want 2 (the already-migrated flows)", res2.FlowsSkipped)
	}
	if res2.FlowsFailed != 0 {
		t.Errorf("rerun: failed %d, want 0", res2.FlowsFailed)
	}
	if len(dst.Flows) != 3 {
		t.Errorf("dst should have all 3 flows after rerun, got %d", len(dst.Flows))
	}
}

func TestMigrator_DestinationUnreachable(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()
	dst.PingErr = context.DeadlineExceeded

	_, err := migration.New(src, dst).Migrate(context.Background())
	if err == nil {
		t.Fatal("expected error when destination unreachable")
	}
}

func TestMigrator_BatchSize(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		src.Flows[id] = &interfaces.FlowDocument{
			ID: id, Name: "Flow " + id, Content: []byte("{}"),
		}
	}

	// Small batch to exercise multiple pages
	res, err := migration.New(src, dst).WithBatchSize(3).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 10 {
		t.Errorf("FlowsMigrated: want 10, got %d", res.FlowsMigrated)
	}
}
