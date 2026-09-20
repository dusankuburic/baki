package migration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
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
	// A completed run must say what it did NOT copy; the status used to read
	// as a full upgrade.
	if len(res.NotMigrated) == 0 {
		t.Error("NotMigrated should list the data kinds this migrator leaves behind")
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

// TestMigrator_Conversations verifies that conversation files on disk are
// discovered, decoded, and written to the destination backend during migration.
func TestMigrator_Conversations(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	convDir := t.TempDir()
	// Seed a conversation file at <convDir>/openai/flow-1.json
	convPath := filepath.Join(convDir, "openai", "flow-1.json")
	if err := os.MkdirAll(filepath.Dir(convPath), 0750); err != nil {
		t.Fatal(err)
	}
	conv := models.ConversationFile{
		Version: 1,
		FlowKey: "flow-1",
		Scope:   "openai",
		Messages: []models.ChatMessage{
			{ID: "m1", Role: "user", Content: "hello", Timestamp: time.Now()},
			{ID: "m2", Role: "assistant", Content: "hi there", Timestamp: time.Now()},
		},
	}
	data, err := json.Marshal(conv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(convPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	res, err := migration.New(src, dst).
		WithConversationsDir(convDir).
		Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.ConversationsMigrated != 1 {
		t.Errorf("ConversationsMigrated: want 1, got %d", res.ConversationsMigrated)
	}
	if res.ConversationsFailed != 0 {
		t.Errorf("ConversationsFailed: want 0, got %d", res.ConversationsFailed)
	}

	saved, sErr := dst.LoadConversation(context.Background(), "flow-1", "openai")
	if sErr != nil {
		t.Fatalf("LoadConversation: %v", sErr)
	}
	if len(saved) != 2 {
		t.Fatalf("saved messages: want 2, got %d", len(saved))
	}
	if saved[0].Content != "hello" || saved[1].Content != "hi there" {
		t.Errorf("message content mismatch: %+v", saved)
	}
}

// TestMigrator_NoConversationsDir verifies the migrator is a no-op for
// conversations when the directory doesn't exist (graceful degradation).
func TestMigrator_NoConversationsDir(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	res, err := migration.New(src, dst).
		WithConversationsDir("/nonexistent/path/conversations").
		Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.ConversationsMigrated != 0 || res.ConversationsFailed != 0 {
		t.Errorf("expected 0 conversations migrated/failed, got %d/%d",
			res.ConversationsMigrated, res.ConversationsFailed)
	}
}

// TestMigrator_MalformedConversationRecordedAsFailure verifies a corrupt
// conversation file is counted as a failure without aborting the run.
func TestMigrator_MalformedConversationRecordedAsFailure(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	convDir := t.TempDir()
	badPath := filepath.Join(convDir, "openai", "bad.json")
	if err := os.MkdirAll(filepath.Dir(badPath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badPath, []byte("{not-json"), 0600); err != nil {
		t.Fatal(err)
	}

	res, err := migration.New(src, dst).
		WithConversationsDir(convDir).
		Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.ConversationsFailed != 1 {
		t.Errorf("ConversationsFailed: want 1, got %d", res.ConversationsFailed)
	}
	if res.ConversationsMigrated != 0 {
		t.Errorf("ConversationsMigrated: want 0, got %d", res.ConversationsMigrated)
	}
}

// TestMigrator_Settings_SkipIfDstNonEmpty guards H2: a re-run after the admin
// has tuned cloud-mode settings must NOT roll them back to the stale source
// values. The migrator should treat dst settings as authoritative once seeded.
func TestMigrator_Settings_SkipIfDstNonEmpty(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	// Source has the "old" settings to migrate.
	srcSettings, _ := src.LoadSettings(context.Background())
	srcSettings.Analysis.Rules = map[string]interfaces.RuleConfig{
		"deep-nesting": {Enabled: false, Severity: "warning"},
	}
	if err := src.SaveSettings(context.Background(), srcSettings); err != nil {
		t.Fatalf("seed src settings: %v", err)
	}

	// Destination already has DIFFERENT settings an admin applied post-migrate.
	dstSettings, _ := dst.LoadSettings(context.Background())
	dstSettings.Analysis.Rules = map[string]interfaces.RuleConfig{
		"deep-nesting": {Enabled: true, Severity: "error"},
	}
	if err := dst.SaveSettings(context.Background(), dstSettings); err != nil {
		t.Fatalf("seed dst settings: %v", err)
	}

	if _, err := migration.New(src, dst).Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	got, _ := dst.LoadSettings(context.Background())
	if rc := got.Analysis.Rules["deep-nesting"]; rc.Severity != "error" || !rc.Enabled {
		t.Errorf("dst settings were overwritten by migration: %+v (want error/enabled)", rc)
	}
}

// TestMigrator_Conversations_SkipIfDstNonEmpty guards H3: a re-run must not
// clobber conversations the user has continued in cloud mode. The migrator
// loads dst first; if non-empty for (flowID, scope), it skips the file.
func TestMigrator_Conversations_SkipIfDstNonEmpty(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	convDir := t.TempDir()
	convPath := filepath.Join(convDir, "openai", "flow-1.json")
	if err := os.MkdirAll(filepath.Dir(convPath), 0750); err != nil {
		t.Fatal(err)
	}
	conv := models.ConversationFile{
		Version: 1, FlowKey: "flow-1", Scope: "openai",
		Messages: []models.ChatMessage{
			{ID: "m1", Role: "user", Content: "OLD: from local mode", Timestamp: time.Now()},
		},
	}
	data, _ := json.Marshal(conv)
	if err := os.WriteFile(convPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	// Destination already has a NEWER conversation the user continued post-migrate.
	if err := dst.SaveConversation(context.Background(), "flow-1", "openai", []interfaces.ChatMessage{
		{ID: "new1", Role: "user", Content: "NEW: continued in cloud", Timestamp: "2026-01-01T00:00:00Z"},
	}); err != nil {
		t.Fatalf("seed dst conversation: %v", err)
	}

	res, err := migration.New(src, dst).
		WithConversationsDir(convDir).
		Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.ConversationsMigrated != 0 {
		t.Errorf("ConversationsMigrated: want 0 (skipped), got %d", res.ConversationsMigrated)
	}
	if res.ConversationsFailed != 0 {
		t.Errorf("ConversationsFailed: want 0, got %d", res.ConversationsFailed)
	}

	// Destination content must be the NEWER conversation, not the overwritten one.
	got, _ := dst.LoadConversation(context.Background(), "flow-1", "openai")
	if len(got) != 1 || got[0].Content != "NEW: continued in cloud" {
		t.Errorf("dst conversation was overwritten: %+v", got)
	}
}

// TestMigrator_AdoptsFlowsWhoseOwnerIsNotInTheDestination: users are not
// migrated, so a legacy flow names an owner the new deployment does not have.
// Stored as-is the flow belongs to nobody — invisible in every library,
// portfolio and search view — while the run reported it as migrated.
func TestMigrator_AdoptsFlowsWhoseOwnerIsNotInTheDestination(t *testing.T) {
	src := testutil.NewFakeBackend()
	// The shared fake has no user store, so the destination is wrapped to
	// answer for the one account that exists there.
	dst := &backendWithUsers{FakeBackend: testutil.NewFakeBackend(), users: map[string]bool{"kept-owner": true}}
	ctx := context.Background()
	for id, owner := range map[string]string{"orphan": "", "ghost": "deleted-user", "kept": "kept-owner"} {
		src.Flows[id] = &interfaces.FlowDocument{
			ID: id, Name: id, OwnerID: owner, Content: []byte(`{"subflows":[]}`),
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
	}

	res, err := migration.New(src, dst).WithDefaultOwner("operator-1").Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 3 {
		t.Fatalf("FlowsMigrated = %d, want 3", res.FlowsMigrated)
	}
	if res.FlowsAdopted != 2 {
		t.Errorf("FlowsAdopted = %d, want 2 (the ownerless and the ghost-owned flow)", res.FlowsAdopted)
	}
	owners := map[string]string{}
	for id, f := range dst.Flows {
		owners[id] = f.OwnerID
	}
	if owners["orphan"] != "operator-1" || owners["ghost"] != "operator-1" {
		t.Errorf("orphaned flows should be adopted by the operator, got %v", owners)
	}
	if owners["kept"] != "kept-owner" {
		t.Errorf("a flow whose owner exists must keep it, got %q", owners["kept"])
	}
}

// TestMigrator_WithoutDefaultOwner_LeavesOwnersAlone keeps the option opt-in.
func TestMigrator_WithoutDefaultOwner_LeavesOwnersAlone(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()
	src.Flows["a"] = &interfaces.FlowDocument{ID: "a", Name: "a", OwnerID: "", Content: []byte(`{"subflows":[]}`)}

	res, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsAdopted != 0 || dst.Flows["a"].OwnerID != "" {
		t.Errorf("adopted=%d owner=%q, want 0 and empty", res.FlowsAdopted, dst.Flows["a"].OwnerID)
	}
}

// backendWithUsers gives the fake destination a user store: the migrator asks
// whether a flow's recorded owner exists there before adopting the flow.
type backendWithUsers struct {
	*testutil.FakeBackend
	users map[string]bool
}

func (b *backendWithUsers) LoadUserByID(_ context.Context, id string) (*interfaces.User, error) {
	if b.users[id] {
		return &interfaces.User{ID: id}, nil
	}
	return nil, interfaces.ErrNotFound
}
