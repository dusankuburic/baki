package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

var snapshotLimits = interfaces.FlowSnapshotLimits{Keep: 20, MaxBytes: 1 << 20}

func (f *r2Fixture) pushSnapshot(ctx context.Context, flowID, id, source string) error {
	f.t.Helper()
	return f.b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: f.id(id), FlowID: flowID, Label: "before " + id, Bytes: int64(len(source)), Source: source}, snapshotLimits)
}

func (f *r2Fixture) snapshotIDs(ctx context.Context, flowID string) string {
	f.t.Helper()
	list, err := f.b.ListFlowSnapshots(ctx, flowID)
	if err != nil {
		f.t.Fatalf("ListFlowSnapshots: %v", err)
	}
	ids := make([]string, len(list))
	for i, s := range list {
		ids[i] = strings.TrimSuffix(strings.TrimPrefix(s.ID, "r2-"), "-"+f.run)
	}
	return strings.Join(ids, ",")
}

// TestMigration0045_Shape pins the step without a database: a new table only (expand), cascading with its flow,
// row-level security enabled and forced with the one policy, and a down step that drops it.
func TestMigration0045_Shape(t *testing.T) {
	m, ok := migrationByVersion(45)
	if !ok {
		t.Fatal("migration v45 not registered")
	}
	if m.name != "flow_snapshots" || m.compat != compatExpand {
		t.Errorf("v45 = %q (%v), want flow_snapshots, expand (the release before never reads the table)", m.name, m.compat)
	}
	up := oneLine(m.sql)
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS flow_snapshots (",
		"flow_id TEXT NOT NULL REFERENCES flows (id) ON DELETE CASCADE",
		"CREATE INDEX IF NOT EXISTS flow_snapshots_flow_seq_idx ON flow_snapshots (flow_id, seq DESC);",
		"ALTER TABLE flow_snapshots ENABLE ROW LEVEL SECURITY;",
		"ALTER TABLE flow_snapshots FORCE ROW LEVEL SECURITY;",
		"CREATE POLICY rls_flow_snapshots_visible ON flow_snapshots FOR ALL",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("v45 lacks %q", want)
		}
	}
	for _, forbidden := range []string{"CONCURRENTLY", "ALTER TABLE FLOWS", "DROP TABLE", "DROP COLUMN"} {
		if strings.Contains(strings.ToUpper(m.sql), forbidden) {
			t.Errorf("v45 must not contain %s", forbidden)
		}
	}
	if down := oneLine(m.downSQL); down == "" || !strings.Contains(down, "DROP TABLE IF EXISTS flow_snapshots;") {
		t.Errorf("v45 down = %q, want it to drop the table", down)
	}
}

// TestMigration0045_UpDownUp: the table exists with its policy (RLS forced), a rollback drops it and a re-apply
// brings it back empty.
func TestMigration0045_UpDownUp(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	exists := func(b *PostgresStorageBackend) (table bool, policies int, forced bool) {
		t.Helper()
		if err := b.db.QueryRowContext(ctx, `SELECT to_regclass('flow_snapshots') IS NOT NULL`).Scan(&table); err != nil {
			t.Fatal(err)
		}
		_ = b.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_policies WHERE tablename = 'flow_snapshots'`).Scan(&policies)
		_ = b.db.QueryRowContext(ctx, `SELECT COALESCE(bool_and(relforcerowsecurity), false) FROM pg_class WHERE relname = 'flow_snapshots'`).Scan(&forced)
		return table, policies, forced
	}
	if table, policies, forced := exists(b); !table || policies != 1 || !forced {
		t.Fatalf("at v45: table=%v policies=%d forced=%v, want the table with one policy and forced RLS", table, policies, forced)
	}
	rolled, err := b.MigrateDown(ctx, 44)
	if err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 45 {
		t.Fatalf("MigrateDown(44) = %v, %v; want a list ending in 45", rolled, err)
	}
	if table, _, _ := exists(b); table {
		t.Fatal("the table survived the rollback")
	}
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("reconnect (re-apply v45): %v", err)
	}
	t.Cleanup(func() { b2.Close() })
	if table, policies, forced := exists(b2); !table || policies != 1 || !forced {
		t.Fatalf("after the re-apply: table=%v policies=%d forced=%v", table, policies, forced)
	}
}

// TestPostgres_FlowSnapshots_VisibleWhereTheFlowIs: under row-level security a snapshot is reachable by the owner,
// a collaborator and an organisation member of its flow and by nobody else; a stranger can neither list, load nor
// push, and the refused push leaves their transaction usable.
func TestPostgres_FlowSnapshots_VisibleWhereTheFlowIs(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 45 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	owner, collab, member, stranger := f.user("owner"), f.user("collab"), f.user("member"), f.user("stranger")
	org := f.org("org", owner, map[string]string{member: "member"})
	shared := f.flow("shared", owner, org, "Shared", "")
	f.collab(shared, collab)
	if err := f.pushSnapshot(f.ctx, shared, "s1", "owner's source"); err != nil {
		t.Fatal(err)
	}

	for name, uid := range map[string]string{"owner": owner, "collab": collab, "member": member} {
		rctx := f.rls(uid)
		if got := f.snapshotIDs(rctx, shared); got != "s1" {
			t.Errorf("%s lists %q, want s1", name, got)
		}
		if s, err := f.b.LoadFlowSnapshot(rctx, shared, f.id("s1")); err != nil || s.Source != "owner's source" {
			t.Errorf("%s loads %+v, %v", name, s, err)
		}
	}

	rctx := f.rls(stranger)
	if got := f.snapshotIDs(rctx, shared); got != "" {
		t.Errorf("a stranger lists %q, want nothing", got)
	}
	if _, err := f.b.LoadFlowSnapshot(rctx, shared, f.id("s1")); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("a stranger loads the snapshot: %v, want ErrNotFound", err)
	}
	if err := f.pushSnapshot(rctx, shared, "s2", "planted"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("a stranger's push = %v, want ErrNotFound", err)
	}
	// The refused push rolled back to its savepoint: the same transaction still answers.
	if got := f.snapshotIDs(rctx, shared); got != "" {
		t.Errorf("after the refused push the stranger lists %q", got)
	}
	if got := f.snapshotIDs(f.ctx, shared); got != "s1" {
		t.Errorf("the planted snapshot reached the table: %q", got)
	}
}

// TestPostgres_FlowSnapshots_JoinTheRequestTransaction: a snapshot pushed inside a request's transaction commits
// and rolls back with it, and a push that fails (the flow is gone) leaves the transaction, and the mutation before
// it, intact.
func TestPostgres_FlowSnapshots_JoinTheRequestTransaction(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 45 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	owner := f.user("owner")
	flow := f.flow("flow", owner, "", "Flow", "")

	tx, err := f.b.BeginRLS(f.ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	rctx := WithRLSTx(f.ctx, tx)
	if err := f.pushSnapshot(rctx, flow, "rolled", "x"); err != nil {
		t.Fatal(err)
	}
	if got := f.snapshotIDs(rctx, flow); got != "rolled" {
		t.Fatalf("the transaction does not see its own snapshot: %q", got)
	}
	if got := f.snapshotIDs(f.ctx, flow); got != "" {
		t.Fatalf("an uncommitted snapshot is visible outside the transaction: %q", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := f.snapshotIDs(f.ctx, flow); got != "" {
		t.Fatalf("a rolled-back request left snapshot %q behind", got)
	}

	tx, err = f.b.BeginRLS(f.ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	rctx = WithRLSTx(f.ctx, tx)
	if err := f.b.SaveConversation(rctx, flow, "flow", []interfaces.ChatMessage{{ID: "m1", Role: "user", Content: "kept"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.pushSnapshot(rctx, f.id("no-such-flow"), "orphan", "x"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Fatalf("push for a missing flow = %v, want ErrNotFound", err)
	}
	if err := f.pushSnapshot(rctx, flow, "committed", "x"); err != nil {
		t.Fatalf("push after a refused one in the same transaction: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit after a refused push: %v", err)
	}
	if got := f.snapshotIDs(f.ctx, flow); got != "committed" {
		t.Errorf("snapshots after the commit = %q, want committed", got)
	}
	if msgs, err := f.b.LoadConversation(f.ctx, flow, "flow"); err != nil || len(msgs) != 1 {
		t.Errorf("the mutation before the refused push was lost: %v, %v", msgs, err)
	}
}

// TestPostgres_FlowSnapshots_SecondBackendSeesTheRing: two backends over one database, as two replicas are, share
// the ring: what one pushes the other lists and loads, and the other's push prunes the first's rows.
func TestPostgres_FlowSnapshots_SecondBackendSeesTheRing(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	other, err := New(context.Background(), DefaultConfig(dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	ctx := context.Background()
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: "replica-flow", Name: "f", OwnerID: "u1", Content: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	lim := interfaces.FlowSnapshotLimits{Keep: 2, MaxBytes: 1 << 20}
	push := func(b *PostgresStorageBackend, id string) {
		t.Helper()
		if err := b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: id, FlowID: "replica-flow", Source: id}, lim); err != nil {
			t.Fatal(err)
		}
	}
	push(b, "a")
	push(other, "b")
	push(b, "c")
	list, err := other.ListFlowSnapshots(ctx, "replica-flow")
	if err != nil || len(list) != 2 || list[0].ID != "b" || list[1].ID != "c" {
		t.Fatalf("the other backend lists %+v, %v; want b, c", list, err)
	}
	if s, err := other.LoadFlowSnapshot(ctx, "replica-flow", "c"); err != nil || s.Source != "c" {
		t.Fatalf("the other backend loads %+v, %v", s, err)
	}
}

// TestPostgres_FlowSnapshots_LifecycleAndPurge: the flow's deletion and the owner's erasure take the ring along, the
// retention sweep deletes snapshots past their 24 hours and reports them, and keeps the live ones.
func TestPostgres_FlowSnapshots_LifecycleAndPurge(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	count := func(where string) (n int) {
		t.Helper()
		if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM flow_snapshots WHERE `+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := b.CreateUser(ctx, &interfaces.User{ID: "snap-owner", Email: "snap-owner@test.local", Password: "x", Role: "member", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"kept-flow", "doomed-flow", "erased-flow"} {
		owner := "u1"
		if id == "erased-flow" {
			owner = "snap-owner"
		}
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: id, Name: id, OwnerID: owner, Content: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		for _, snap := range []string{"live", "old"} {
			s := &interfaces.FlowSnapshot{ID: id + "-" + snap, FlowID: id, Source: "x"}
			if snap == "old" {
				s.CreatedAt = time.Now().Add(-interfaces.FlowSnapshotTTL - time.Minute)
			}
			if err := b.PushFlowSnapshot(ctx, s, snapshotLimits); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := count("true"); n != 6 {
		t.Fatalf("seeded %d snapshots, want 6", n)
	}

	res, err := b.PurgeExpiredData(ctx, 0)
	if err != nil {
		t.Fatalf("PurgeExpiredData: %v", err)
	}
	if res.FlowSnapshots != 3 {
		t.Errorf("the sweep reported %d snapshots, want the 3 expired", res.FlowSnapshots)
	}
	if n := count("created_at < NOW() - INTERVAL '24 hours'"); n != 0 {
		t.Errorf("%d expired snapshots survived the sweep", n)
	}
	if n := count("true"); n != 3 {
		t.Errorf("%d snapshots left, want the 3 live ones", n)
	}

	if err := b.DeleteFlow(ctx, "doomed-flow"); err != nil {
		t.Fatal(err)
	}
	if n := count("flow_id = 'doomed-flow'"); n != 0 {
		t.Errorf("the deleted flow's ring has %d rows", n)
	}
	if err := b.DeleteUser(ctx, "snap-owner"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if n := count("flow_id = 'erased-flow'"); n != 0 {
		t.Errorf("the erased owner's flow left %d snapshots", n)
	}
	if n := count("flow_id = 'kept-flow'"); n != 1 {
		t.Errorf("another owner's flow has %d snapshots, want its live one", n)
	}
}
