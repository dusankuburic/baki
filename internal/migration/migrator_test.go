package migration_test

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/filesystem"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// openSource reads d through the read-only source production uses, so these tests run on files laid out as an old
// build left them.
func openSource(t *testing.T, d *contract.LegacyDir) migration.LegacySource {
	t.Helper()
	src, err := filesystem.OpenLegacySource(d.Root)
	if err != nil {
		t.Fatalf("OpenLegacySource: %v", err)
	}
	return src
}

func flowDoc(id, name, owner string) *interfaces.FlowDocument {
	return &interfaces.FlowDocument{
		ID: id, Name: name, OwnerID: owner, Content: json.RawMessage(`{"subflows":[]}`),
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

func TestMigrator_MigratesFlows(t *testing.T) {
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()
	for i := 0; i < 3; i++ {
		id := string(rune('a' + i))
		d.Flow(flowDoc(id, "Flow "+id, ""))
	}
	d.Settings(&interfaces.AppSettings{Version: 1})

	res, err := migration.New(openSource(t, d), dst).Migrate(context.Background())
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
	// as a full upgrade. The list is part of the status JSON.
	want := []string{
		"users and sessions", "organisations and memberships", "API tokens", "finding triage and comments",
		"flow versions", "share links", "governance alerts", "audit events",
	}
	if !slices.Equal(res.NotMigrated, want) {
		t.Errorf("NotMigrated = %q, want %q", res.NotMigrated, want)
	}
}

func TestMigrator_SkipsAlreadyMigrated(t *testing.T) {
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()

	flow := flowDoc("existing", "Existing", "")
	d.Flow(flow)
	dst.Flows["existing"] = flow // already there

	res, err := migration.New(openSource(t, d), dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 0 || res.FlowsSkipped != 1 {
		t.Errorf("migrated/skipped = %d/%d, want 0/1 (already present)", res.FlowsMigrated, res.FlowsSkipped)
	}
}

// TestMigrator_KeepsFlowTimes: a copied flow keeps its created and updated times. The copy went through SaveFlow,
// which stamped every flow with the time of the run, so the library sorted all of them as just edited.
func TestMigrator_KeepsFlowTimes(t *testing.T) {
	d := contract.NewLegacyDir(t)
	created := time.Date(2023, 4, 5, 6, 7, 8, 0, time.UTC)
	updated := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	doc := flowDoc("timed", "Timed", "")
	doc.CreatedAt, doc.UpdatedAt, doc.Version = created, updated, 5
	d.Flow(doc)
	dst := testutil.NewFakeBackend()

	res, err := migration.New(openSource(t, d), dst).Migrate(context.Background())
	if err != nil || res.FlowsMigrated != 1 {
		t.Fatalf("Migrate = %+v, %v; want one flow copied", res, err)
	}
	got := dst.Flows["timed"]
	if got == nil || !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(updated) || got.Version != 0 {
		t.Errorf("copied flow = %+v; want created %v, updated %v, version 0", got, created, updated)
	}
}

// headerBlind hides every stored flow from the migrator's existence check, as for a flow stored between that check
// and the copy.
type headerBlind struct{ *testutil.FakeBackend }

func (headerBlind) LoadFlowHeader(context.Context, string) (*interfaces.FlowDocument, error) {
	return nil, interfaces.ErrNotFound
}

// TestMigrator_FlowStoredAfterTheCheckIsSkipped: the copy never overwrites a stored flow. It is counted as skipped,
// like one the existence check found, and its owner is not counted as adopted.
func TestMigrator_FlowStoredAfterTheCheckIsSkipped(t *testing.T) {
	d := contract.NewLegacyDir(t)
	d.Flow(flowDoc("raced", "From the source", "ghost"))
	dst := testutil.NewFakeBackend()
	dst.Flows["raced"] = &interfaces.FlowDocument{ID: "raced", Name: "Stored", OwnerID: "someone", Version: 4}

	res, err := migration.New(openSource(t, d), headerBlind{dst}).WithDefaultOwner("operator-1").Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsSkipped != 1 || res.FlowsMigrated != 0 || res.FlowsFailed != 0 || res.FlowsAdopted != 0 {
		t.Errorf("result = %+v; want the flow skipped and nothing adopted", res)
	}
	if got := dst.Flows["raced"]; got.Name != "Stored" || got.OwnerID != "someone" || got.Version != 4 {
		t.Errorf("stored flow = %+v; want it untouched", got)
	}
}

func TestMigrator_InvalidFlowRecordedAsFailure(t *testing.T) {
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()
	d.File("flows/bad.json", []byte(`{"id": "bad", "name": "", "content": {}}`)) // invalid: no name

	res, err := migration.New(openSource(t, d), dst).Migrate(context.Background())
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

// TestMigrator_UndecodableFlowIsReportedNotSkipped: the filesystem backend's
// listing skipped a flow file it could not decode, so the run never mentioned
// it. The legacy source lists it by name and the copy reports it.
func TestMigrator_UndecodableFlowIsReportedNotSkipped(t *testing.T) {
	d := contract.NewLegacyDir(t)
	d.Flow(flowDoc("good", "Good", ""))
	d.File("flows/corrupt.json", []byte(`{"id": "corrupt", "name": `))

	res, err := migration.New(openSource(t, d), testutil.NewFakeBackend()).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 1 || res.FlowsFailed != 1 {
		t.Fatalf("migrated/failed = %d/%d, want 1/1", res.FlowsMigrated, res.FlowsFailed)
	}
	if len(res.Errors) != 1 || res.Errors[0].FlowID != "corrupt" || !strings.HasPrefix(res.Errors[0].Message, "load from source: ") {
		t.Errorf("errors = %+v", res.Errors)
	}
}

// TestMigrator_PartialFailure_RerunCompletes locks in the documented
// re-run model: when a first attempt fails some flows because the
// destination wasn't ready for them, fixing the underlying issue and
// re-running the migrator must complete the partial state without
// re-doing the already-migrated rows.
func TestMigrator_PartialFailure_RerunCompletes(t *testing.T) {
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()

	// Seed three flows; one has no name, which the validator rejects.
	d.Flow(flowDoc("ok-1", "Good Flow 1", ""))
	d.Flow(flowDoc("bad", "", ""))
	d.Flow(flowDoc("ok-2", "Good Flow 2", ""))

	// First run: 2 succeed, 1 fails validation.
	res, err := migration.New(openSource(t, d), dst).Migrate(context.Background())
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
	d.Flow(flowDoc("bad", "Fixed Name", ""))

	res2, err := migration.New(openSource(t, d), dst).Migrate(context.Background())
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
	dst := testutil.NewFakeBackend()
	dst.PingErr = context.DeadlineExceeded

	_, err := migration.New(openSource(t, contract.NewLegacyDir(t)), dst).Migrate(context.Background())
	if err == nil {
		t.Fatal("expected error when destination unreachable")
	}
}

func TestMigrator_BatchSize(t *testing.T) {
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		d.Flow(flowDoc(id, "Flow "+id, ""))
	}

	// Small batch to exercise multiple pages
	res, err := migration.New(openSource(t, d), dst).WithBatchSize(3).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 10 {
		t.Errorf("FlowsMigrated: want 10, got %d", res.FlowsMigrated)
	}
}

// TestMigrator_Conversations verifies that stored conversations are listed,
// decoded, and written to the destination backend during migration.
func TestMigrator_Conversations(t *testing.T) {
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()
	d.Conversation("openai", "flow-1",
		models.ChatMessage{ID: "m1", Role: "user", Content: "hello", Timestamp: time.Now()},
		models.ChatMessage{ID: "m2", Role: "assistant", Content: "hi there", Timestamp: time.Now()},
	)

	res, err := migration.New(openSource(t, d), dst).Migrate(context.Background())
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
	res, err := migration.New(openSource(t, contract.NewLegacyDir(t)), testutil.NewFakeBackend()).Migrate(context.Background())
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
	d := contract.NewLegacyDir(t)
	d.File("conversations/openai/bad.json", []byte("{not-json"))
	d.Conversation("openai", "good", models.ChatMessage{ID: "m1", Role: "user", Content: "hi"})

	res, err := migration.New(openSource(t, d), testutil.NewFakeBackend()).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.ConversationsFailed != 1 {
		t.Errorf("ConversationsFailed: want 1, got %d", res.ConversationsFailed)
	}
	if res.ConversationsMigrated != 1 {
		t.Errorf("ConversationsMigrated: want 1 (the good file after the bad one), got %d", res.ConversationsMigrated)
	}
	if len(res.Errors) != 1 || res.Errors[0].FlowID != "bad.json" || !strings.HasPrefix(res.Errors[0].Message, "conversation: ") {
		t.Errorf("errors = %+v, want one entry named after the file", res.Errors)
	}
}

// TestMigrator_Settings_SkipIfDstNonEmpty guards H2: a re-run after the admin
// has tuned cloud-mode settings must NOT roll them back to the stale source
// values. The migrator should treat dst settings as authoritative once seeded.
func TestMigrator_Settings_SkipIfDstNonEmpty(t *testing.T) {
	ctx := context.Background()
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()

	// Source has the "old" settings to migrate.
	d.Settings(&interfaces.AppSettings{Version: 1, Analysis: interfaces.AnalysisSettings{Rules: map[string]interfaces.RuleConfig{
		"deep-nesting": {Enabled: false, Severity: "warning"},
	}}})

	// Destination already has DIFFERENT settings an admin applied post-migrate.
	dstSettings, _ := dst.LoadSettings(ctx)
	dstSettings.Analysis.Rules = map[string]interfaces.RuleConfig{
		"deep-nesting": {Enabled: true, Severity: "error"},
	}
	if err := dst.SaveSettings(ctx, dstSettings); err != nil {
		t.Fatalf("seed dst settings: %v", err)
	}

	if _, err := migration.New(openSource(t, d), dst).Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	got, _ := dst.LoadSettings(ctx)
	if rc := got.Analysis.Rules["deep-nesting"]; rc.Severity != "error" || !rc.Enabled {
		t.Errorf("dst settings were overwritten by migration: %+v (want error/enabled)", rc)
	}
}

// TestMigrator_Settings_NoSourceFileLeavesDestinationAlone: the filesystem
// backend answered a missing settings.json with defaults, which a run then
// wrote over a destination that had no rule overrides or recent files yet (an
// admin's AI provider choice included) and reported as moved.
func TestMigrator_Settings_NoSourceFileLeavesDestinationAlone(t *testing.T) {
	ctx := context.Background()
	dst := testutil.NewFakeBackend()
	if err := dst.SaveSettings(ctx, &interfaces.AppSettings{Version: 1, AI: interfaces.AISettings{ActiveProvider: "azure-foundry"}}); err != nil {
		t.Fatal(err)
	}

	res, err := migration.New(openSource(t, contract.NewLegacyDir(t)), dst).Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.SettingsMoved {
		t.Error("SettingsMoved = true for a source without settings")
	}
	if got, _ := dst.LoadSettings(ctx); got.AI.ActiveProvider != "azure-foundry" {
		t.Errorf("destination settings replaced: active provider %q", got.AI.ActiveProvider)
	}
}

// TestMigrator_Conversations_SkipIfDstNonEmpty guards H3: a re-run must not
// clobber conversations the user has continued in cloud mode. The migrator
// loads dst first; if non-empty for (flowID, scope), it skips the file.
func TestMigrator_Conversations_SkipIfDstNonEmpty(t *testing.T) {
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()
	d.Conversation("openai", "flow-1",
		models.ChatMessage{ID: "m1", Role: "user", Content: "OLD: from local mode", Timestamp: time.Now()},
	)

	// Destination already has a NEWER conversation the user continued post-migrate.
	if err := dst.SaveConversation(context.Background(), "flow-1", "openai", []interfaces.ChatMessage{
		{ID: "new1", Role: "user", Content: "NEW: continued in cloud", Timestamp: "2026-01-01T00:00:00Z"},
	}); err != nil {
		t.Fatalf("seed dst conversation: %v", err)
	}

	res, err := migration.New(openSource(t, d), dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.ConversationsMigrated != 0 || res.ConversationsSkipped != 1 {
		t.Errorf("migrated/skipped = %d/%d, want 0/1", res.ConversationsMigrated, res.ConversationsSkipped)
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
	d := contract.NewLegacyDir(t)
	// The destination is wrapped to answer for the one account that exists there.
	dst := &backendWithUsers{FakeBackend: testutil.NewFakeBackend(), users: map[string]bool{"kept-owner": true}}
	ctx := context.Background()
	for id, owner := range map[string]string{"orphan": "", "ghost": "deleted-user", "kept": "kept-owner"} {
		d.Flow(flowDoc(id, id, owner))
	}

	res, err := migration.New(openSource(t, d), dst).WithDefaultOwner("operator-1").Migrate(ctx)
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
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()
	d.Flow(flowDoc("a", "a", ""))

	res, err := migration.New(openSource(t, d), dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsAdopted != 0 || dst.Flows["a"].OwnerID != "" {
		t.Errorf("adopted=%d owner=%q, want 0 and empty", res.FlowsAdopted, dst.Flows["a"].OwnerID)
	}
}

// TestMigrator_ReadOnlyDataDirStillMigrates: the source never writes to the
// legacy directory (the filesystem backend created it and swept temp files on
// open), so a directory mounted read-only migrates in full and is left exactly
// as it was.
func TestMigrator_ReadOnlyDataDirStillMigrates(t *testing.T) {
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()
	for _, id := range []string{"f1", "f2", "f3"} {
		d.Flow(flowDoc(id, "Flow "+id, ""))
	}
	d.Settings(&interfaces.AppSettings{Version: 1})
	d.Conversation("openai", "f1", models.ChatMessage{ID: "m1", Role: "user", Content: "hi", Timestamp: time.Now()})
	if !d.MakeReadOnly() {
		t.Log("this OS lets the test create files in a read-only directory; the snapshot comparison still proves nothing was written")
	}
	before := d.Snapshot()

	res, err := migration.New(openSource(t, d), dst).WithBatchSize(2).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 3 || !res.SettingsMoved || res.ConversationsMigrated != 1 || len(res.Errors) != 0 {
		t.Errorf("result = %+v, want 3 flows, settings and 1 conversation", res)
	}
	if after := d.Snapshot(); !maps.Equal(before, after) {
		t.Errorf("the legacy directory changed:\nbefore %v\nafter  %v", before, after)
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

// TestMigrator_ConversationsAreTheImportersAndNeverOverwriteAnOwnedThread: a desktop thread is imported as the
// importing user's (their thread index lists it; R-security-3), and an owned conversation already under the key —
// even an empty one, a thread named before its first turn — is never replaced.
func TestMigrator_ConversationsAreTheImportersAndNeverOverwriteAnOwnedThread(t *testing.T) {
	d := contract.NewLegacyDir(t)
	dst := testutil.NewFakeBackend()
	ctx := context.Background()
	const fresh, named = "0b7c4c1e-6a65-4cf4-9a49-6f7d3b1f2a10", "1b7c4c1e-6a65-4cf4-9a49-6f7d3b1f2a10"
	d.Conversation(fresh, "flow-1", models.ChatMessage{ID: "m1", Role: "user", Content: "from the desktop", Timestamp: time.Now()})
	d.Conversation(named, "flow-1", models.ChatMessage{ID: "m2", Role: "user", Content: "would overwrite", Timestamp: time.Now()})
	v, err := dst.SetConversationTitle(ctx, "flow-1", named, "operator-1", "named in the browser")
	if err != nil {
		t.Fatal(err)
	}

	res, err := migration.New(openSource(t, d), dst).WithDefaultOwner("operator-1").Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.ConversationsMigrated != 1 || res.ConversationsSkipped != 1 {
		t.Errorf("migrated %d, skipped %d; want 1 and 1", res.ConversationsMigrated, res.ConversationsSkipped)
	}
	threads, err := dst.ListConversations(ctx, "flow-1", "operator-1", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]interfaces.ConversationSummary{}
	for _, c := range threads {
		byKey[c.Scope] = c
	}
	if c, ok := byKey[fresh]; !ok || c.MessageCount != 1 {
		t.Errorf("the imported thread is not the importer's: %+v", threads)
	}
	if c := byKey[named]; c.Title != "named in the browser" || c.MessageCount != 0 || c.Version != v {
		t.Errorf("the owned empty thread = %+v, want it untouched (title kept, no messages, version %d)", c, v)
	}
}
