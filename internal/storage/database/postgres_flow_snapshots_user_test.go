package database

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestMigration0047_Shape pins the step without a database: a column with a constant default (the release before
// inserts without it and gets an empty user), a partial index for the per-user trim, nothing dropped or rewritten, and a down
// step that removes both.
func TestMigration0047_Shape(t *testing.T) {
	m, ok := migrationByVersion(47)
	if !ok {
		t.Fatal("migration v47 not registered")
	}
	if m.name != "flow_snapshots_user" || m.compat != compatExpand {
		t.Errorf("v47 = %q (%v), want flow_snapshots_user, expand (the release before never reads the column)", m.name, m.compat)
	}
	up := oneLine(m.sql)
	for _, want := range []string{
		"ALTER TABLE flow_snapshots ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';",
		"CREATE INDEX IF NOT EXISTS flow_snapshots_user_seq_idx ON flow_snapshots (user_id, seq DESC) WHERE user_id <> '';",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("v47 lacks %q", want)
		}
	}
	for _, forbidden := range []string{"CONCURRENTLY", "DROP", "UPDATE ", "POLICY"} {
		if strings.Contains(strings.ToUpper(m.sql), forbidden) {
			t.Errorf("v47 must not contain %s", forbidden)
		}
	}
	down := oneLine(m.downSQL)
	for _, want := range []string{"DROP INDEX IF EXISTS flow_snapshots_user_seq_idx;", "ALTER TABLE flow_snapshots DROP COLUMN IF EXISTS user_id;"} {
		if !strings.Contains(down, want) {
			t.Errorf("v47 down = %q, want it to contain %q", down, want)
		}
	}
}

// TestMigration0047_UpDownUp: the column and index exist, a rollback removes them and keeps the snapshots, a
// re-apply brings them back with an empty user on the rows pushed meanwhile.
func TestMigration0047_UpDownUp(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	shape := func(b *PostgresStorageBackend) (column, index bool) {
		t.Helper()
		if err := b.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'flow_snapshots' AND column_name = 'user_id')`).Scan(&column); err != nil {
			t.Fatal(err)
		}
		if err := b.db.QueryRowContext(ctx, `SELECT to_regclass('flow_snapshots_user_seq_idx') IS NOT NULL`).Scan(&index); err != nil {
			t.Fatal(err)
		}
		return column, index
	}
	if column, index := shape(b); !column || !index {
		t.Fatalf("at v47: column=%v index=%v, want both", column, index)
	}
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: "mig47-flow", Name: "f", OwnerID: "u1", Content: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: "mig47-a", FlowID: "mig47-flow", UserID: "u1", Source: "a"}, snapshotLimits); err != nil {
		t.Fatal(err)
	}
	rolled, err := b.MigrateDown(ctx, 46)
	// Later steps are rolled back too, newest first; v47 is the last.
	if err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 47 {
		t.Fatalf("MigrateDown(46) = %v, %v; want a list ending in 47", rolled, err)
	}
	if column, index := shape(b); column || index {
		t.Fatalf("after the rollback: column=%v index=%v, want neither", column, index)
	}
	// The release before inserts without the column.
	if _, err := b.db.ExecContext(ctx, `INSERT INTO flow_snapshots (id, flow_id, source) VALUES ('mig47-b', 'mig47-flow', 'b')`); err != nil {
		t.Fatalf("an insert of the release before at v46: %v", err)
	}
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("reconnect (re-apply v47): %v", err)
	}
	t.Cleanup(func() { b2.Close() })
	if column, index := shape(b2); !column || !index {
		t.Fatalf("after the re-apply: column=%v index=%v", column, index)
	}
	var users string
	if err := b2.db.QueryRowContext(ctx, `SELECT string_agg(id || '=' || user_id, ',' ORDER BY seq) FROM flow_snapshots WHERE flow_id = 'mig47-flow'`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != "mig47-a=,mig47-b=" {
		t.Errorf("after down and up the rows are %q, want both kept with no user", users)
	}
}

// TestPostgres_FlowSnapshots_UserBoundUnderRLS: inside a collaborator's request the push records the collaborator
// and trims the collaborator's snapshots on every flow they can open, the owner's flow and their own alike, but
// never another user's, and never a row of theirs on a flow they can no longer see (that one ages out in 24 hours).
func TestPostgres_FlowSnapshots_UserBoundUnderRLS(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 47 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	owner, collab := f.user("owner"), f.user("collab")
	shared := f.flow("shared", owner, "", "Shared", "")
	f.collab(shared, collab)
	mine := f.flow("mine", collab, "", "Mine", "")
	hidden := f.flow("hidden", owner, "", "Hidden", "")
	// A snapshot of the collaborator's on a flow they cannot open now (seeded without a scope).
	if err := f.b.PushFlowSnapshot(f.ctx, &interfaces.FlowSnapshot{ID: f.id("h1"), FlowID: hidden, UserID: collab, Source: "h"}, snapshotLimits); err != nil {
		t.Fatal(err)
	}

	lim := interfaces.FlowSnapshotLimits{Keep: 20, MaxBytes: 1 << 20, UserKeep: 2, UserMaxBytes: 1 << 20}
	push := func(ctx context.Context, flowID, user, id string) {
		t.Helper()
		if err := f.b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: f.id(id), FlowID: flowID, UserID: user, Source: id}, lim); err != nil {
			t.Fatalf("push %s: %v", id, err)
		}
	}
	push(f.ctx, shared, owner, "o1")
	rctx := f.rls(collab)
	push(rctx, shared, collab, "c1")
	push(rctx, mine, collab, "c2")
	push(rctx, shared, collab, "c3")
	if got := f.snapshotIDs(rctx, shared); got != "o1,c3" {
		t.Errorf("the shared flow lists %q, want the owner's o1 and the collaborator's newest c3", got)
	}
	if got := f.snapshotIDs(rctx, mine); got != "c2" {
		t.Errorf("the collaborator's flow lists %q, want c2", got)
	}
	if got := f.snapshotIDs(f.ctx, hidden); got != "h1" {
		t.Errorf("the flow the collaborator cannot see lists %q, want h1 untouched", got)
	}
	var who string
	if err := f.b.query(rctx).QueryRowContext(rctx, `SELECT user_id FROM flow_snapshots WHERE id = $1`, f.id("c3")).Scan(&who); err != nil || who != collab {
		t.Errorf("c3 records user %q, %v; want the collaborator", who, err)
	}
}

// TestFlowSnapshotUserTrim_SkipsLockedRows pins, without a database, that the per-user trim takes only the rows no
// other transaction holds: it is best effort, so it never waits on a concurrent push and cannot close a wait cycle.
func TestFlowSnapshotUserTrim_SkipsLockedRows(t *testing.T) {
	q := oneLine(flowSnapshotUserTrimSQL)
	if !strings.Contains(q, "FOR UPDATE SKIP LOCKED") {
		t.Errorf("the user trim does not skip locked rows: %s", q)
	}
	if strings.Contains(oneLine(flowSnapshotFlowTrimSQL), "SKIP LOCKED") {
		t.Error("the per-flow trim skips rows: it must keep the flow's ring exact and wait for a concurrent push instead")
	}
}

// TestPostgres_FlowSnapshots_CrossedTrimsDoNotDeadlock: two pushes whose per-flow trim and user trim reach each other's
// rows, in two request transactions (a user's two tabs, or two users on shared flows). T2 holds U's old row on F2 (its
// per-flow trim deleted it); T1's per-flow trim deletes V's old row on F1 and its user trim reaches U's row that T2
// holds; T2's next push on F1 then needs V's row that T1 holds. With a user trim that waited, that was a deadlock
// (40P01) and a lost undo point; the user trim now skips the row T2 holds, so only T2 waits, for T1's commit.
func TestPostgres_FlowSnapshots_CrossedTrimsDoNotDeadlock(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 47 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	u, v := f.user("u"), f.user("v")
	f1 := f.flow("f1", u, "", "F1", "")
	f2 := f.flow("f2", u, "", "F2", "")
	f.collab(f1, v)
	f.collab(f2, v)
	tight := interfaces.FlowSnapshotLimits{Keep: 1, MaxBytes: 1 << 20, UserKeep: 1, UserMaxBytes: 1 << 20}
	loose := interfaces.FlowSnapshotLimits{Keep: 1, MaxBytes: 1 << 20, UserKeep: 100, UserMaxBytes: 1 << 20}
	push := func(ctx context.Context, flowID, user, id string, lim interfaces.FlowSnapshotLimits) error {
		return f.b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: f.id(id), FlowID: flowID, UserID: user, Source: id}, lim)
	}
	if err := push(f.ctx, f1, v, "v-old", loose); err != nil {
		t.Fatal(err)
	}
	if err := push(f.ctx, f2, u, "u-old", loose); err != nil {
		t.Fatal(err)
	}
	begin := func(user string) (context.Context, *sql.Tx) {
		tx, err := f.b.BeginRLS(f.ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		return WithRLSTx(f.ctx, tx), tx
	}
	ctx2, tx2 := begin(v)
	if err := push(ctx2, f2, v, "v-new", loose); err != nil { // deletes u-old: T2 holds it
		t.Fatal(err)
	}
	ctx1, tx1 := begin(u)
	t1 := make(chan error, 1)
	go func() { t1 <- push(ctx1, f1, u, "u-new", tight) }() // deletes v-old; its user trim reaches u-old
	var t1err error
	t1done := false
	select {
	case t1err = <-t1:
		t1done = true
	case <-time.After(500 * time.Millisecond):
		// A user trim that waits on T2: let T2 close the cycle, which Postgres breaks by failing one of them.
	}
	t2 := make(chan error, 1)
	go func() { t2 <- push(ctx2, f1, v, "v-next", loose) }() // needs v-old, which T1 holds
	// The commit waits for T1's push if it is still running.
	if err := tx1.Commit(); err != nil {
		t.Fatalf("T1 commit: %v", err)
	}
	if !t1done {
		t1err = <-t1
	}
	if t1err != nil {
		t.Fatalf("T1's push: %v", t1err)
	}
	select {
	case err := <-t2:
		if err != nil {
			t.Fatalf("T2's push on F1 after T1 committed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("T2's push on F1 still waits after T1 committed")
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("T2 commit: %v", err)
	}
	if got := f.snapshotIDs(f.ctx, f1); !strings.HasSuffix(got, "v-next") {
		t.Errorf("F1 lists %q, want it to end with v-next", got)
	}
}

// TestPostgres_FlowSnapshots_UserTrimNeverWaits: a row of the user's that another transaction holds is skipped by the
// user trim (the bound is best effort: it may hold one more row until the next push) and the push does not wait.
func TestPostgres_FlowSnapshots_UserTrimNeverWaits(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 47 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	u := f.user("u")
	f1 := f.flow("f1", u, "", "F1", "")
	f2 := f.flow("f2", u, "", "F2", "")
	lim := interfaces.FlowSnapshotLimits{Keep: 20, MaxBytes: 1 << 20, UserKeep: 1, UserMaxBytes: 1 << 20}
	if err := f.b.PushFlowSnapshot(f.ctx, &interfaces.FlowSnapshot{ID: f.id("old"), FlowID: f2, UserID: u, Source: "old"}, lim); err != nil {
		t.Fatal(err)
	}
	holder, err := f.b.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(f.ctx, `SELECT 1 FROM flow_snapshots WHERE id = $1 FOR UPDATE`, f.id("old")); err != nil {
		t.Fatal(err)
	}
	rctx := f.rls(u)
	ctx, cancel := context.WithTimeout(rctx, 3*time.Second)
	defer cancel()
	if err := f.b.PushFlowSnapshot(ctx, &interfaces.FlowSnapshot{ID: f.id("new"), FlowID: f1, UserID: u, Source: "new"}, lim); err != nil {
		t.Fatalf("a push whose user trim reaches a row another transaction holds: %v (it must skip it, not wait)", err)
	}
	if got := f.snapshotIDs(rctx, f2); got != "old" {
		t.Errorf("F2 lists %q, want the held row kept for now", got)
	}
	_ = holder.Rollback()
	if err := f.b.PushFlowSnapshot(rctx, &interfaces.FlowSnapshot{ID: f.id("newer"), FlowID: f1, UserID: u, Source: "newer"}, lim); err != nil {
		t.Fatal(err)
	}
	if got := f.snapshotIDs(rctx, f2) + "|" + f.snapshotIDs(rctx, f1); got != "|newer" {
		t.Errorf("after the holder let go: F2|F1 = %q, want the bound caught up (|newer)", got)
	}
}
