package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// TestMigration0052_Shape pins the v52 step without a database: expand-compatible, nullable columns without a
// default on the existing rows (metadata only), the created_at default set separately (later inserts only), no index
// built in the step (SchemaUpkeep builds conversations_owner_idx concurrently), the owner-scoped policies, and a down
// step that restores v27's policy and drops the columns.
func TestMigration0052_Shape(t *testing.T) {
	m, ok := migrationByVersion(52)
	if !ok {
		t.Fatal("migration v52 not registered")
	}
	if m.name != "conversation_owner" {
		t.Errorf("v52 name = %q, want conversation_owner", m.name)
	}
	if m.compat != compatExpand {
		t.Errorf("v52 compat = %v, want expand (the release before never names the columns; its chat routes run outside RLS)", m.compat)
	}
	up := oneLine(m.sql)
	for _, want := range []string{
		"ADD COLUMN IF NOT EXISTS owner_id TEXT,",
		"ADD COLUMN IF NOT EXISTS title TEXT,",
		"ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ,",
		"ADD COLUMN IF NOT EXISTS message_count INTEGER;",
		"ALTER TABLE conversations ALTER COLUMN created_at SET DEFAULT NOW();",
		"NEW.message_count :=",
		"CREATE POLICY rls_conversations_visible ON conversations FOR SELECT",
		"CREATE POLICY rls_conversations_insert ON conversations FOR INSERT",
		"CREATE POLICY rls_conversations_update ON conversations FOR UPDATE",
		"CREATE POLICY rls_conversations_delete ON conversations FOR DELETE",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("v52 lacks %q", want)
		}
	}
	var code strings.Builder // the statements, without the comments
	for _, line := range strings.Split(m.sql, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code.WriteString(line + "\n")
		}
	}
	for _, forbidden := range []string{"CREATE INDEX", "CONCURRENTLY", "UPDATE CONVERSATIONS SET", "DROP CONSTRAINT", "PRIMARY KEY"} {
		if strings.Contains(strings.ToUpper(code.String()), forbidden) {
			t.Errorf("v52 must not contain %s (a backfill, a lock-holding index build or a key change has no place in an expand step)", forbidden)
		}
	}
	down := oneLine(m.downSQL)
	for _, want := range []string{
		"CREATE POLICY rls_conversations_visible ON conversations FOR ALL",
		"DROP POLICY IF EXISTS rls_conversations_delete ON conversations;",
		"DROP INDEX IF EXISTS conversations_owner_idx;",
		"DROP COLUMN IF EXISTS owner_id;",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("v52 down lacks %q", want)
		}
	}
	found := false
	for _, ix := range schemaUpkeepIndexes {
		if ix.name == "conversations_owner_idx" {
			found = ix.since == 52 && strings.Contains(ix.ddl, "CONCURRENTLY") && strings.Contains(ix.ddl, "WHERE owner_id IS NOT NULL")
		}
	}
	if !found {
		t.Error("SchemaUpkeep does not build conversations_owner_idx (v52, concurrently, partial on owned rows)")
	}
}

// TestMigration0052_Policies pins the catalog after v52: conversations has its four command policies; reading,
// creating and changing need the row the caller's or unowned (and the flow visible); deleting keeps v27's flow-wide
// rule, for the flow's deletion.
func TestMigration0052_Policies(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	if v, _ := b.CurrentSchemaVersion(context.Background()); v < 52 {
		t.Skipf("DB at v%d; need v52", v)
	}
	pols := tablePolicies(t, b, "conversations")
	byCmd := map[string]catalogPolicy{}
	for _, p := range pols {
		byCmd[p.cmd] = p
	}
	if len(pols) != 4 {
		t.Fatalf("conversations has %d policies, want 4: %+v", len(pols), pols)
	}
	for cmd, name := range map[string]string{"SELECT": "rls_conversations_visible", "INSERT": "rls_conversations_insert", "UPDATE": "rls_conversations_update", "DELETE": "rls_conversations_delete"} {
		p, ok := byCmd[cmd]
		if !ok || p.name != name {
			t.Errorf("FOR %s policy = %+v, want %s", cmd, p, name)
			continue
		}
		for i, body := range []string{p.qual, p.check} {
			if body == "" {
				continue
			}
			if !strings.Contains(body, "f.id = conversations.flow_id") {
				t.Errorf("%s does not require the flow visible: %q", name, body)
			}
			mine, unowned := strings.Contains(body, "owner_id = app_current_user_id()"), strings.Contains(body, "owner_id IS NULL")
			switch {
			case cmd == "DELETE" && strings.Contains(body, "owner_id"):
				t.Errorf("%s names the owner; the flow's deletion must reach every user's threads: %q", name, body)
			case cmd == "UPDATE" && i == 1 && (!mine || unowned):
				// An update leaves the row the caller's: it may claim an unowned row, never un-own one.
				t.Errorf("%s WITH CHECK must require the caller as owner, without the unowned case: %q", name, body)
			case cmd != "DELETE" && (cmd != "UPDATE" || i != 1) && (!mine || !unowned):
				t.Errorf("%s is not owner-scoped: %q", name, body)
			}
		}
	}
}

// TestMigration0052_OwnersUnderRLS: users A and B are members of an organisation whose flow they chat about. Under
// B's row-level-security user, B sees B's own conversation and the unowned one, never A's; cannot give a row to A or
// change A's; the flow owner's deletion of the flow (under their user) removes every user's threads. The statements
// of the release before (an upsert naming no owner) still work, create unowned rows, and the trigger counts them.
func TestMigration0052_OwnersUnderRLS(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if v, _ := b.CurrentSchemaVersion(ctx); v < 52 {
		t.Skipf("DB at v%d; need v52", v)
	}
	owner, userA, userB := "m52-owner", "m52-a", "m52-b"
	for _, id := range []string{owner, userA, userB} {
		if err := b.CreateUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "hash", Role: "member"}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: "m52-org", Name: "Org", OwnerID: owner, Members: []interfaces.OrgMember{
		{UserID: owner, Role: "admin"}, {UserID: userA, Role: "member"}, {UserID: userB, Role: "member"},
	}}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	const flowID = "m52-flow"
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: flowID, Name: "F", OwnerID: owner, OrganizationID: "m52-org", Content: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	msgs := []interfaces.ChatMessage{{ID: "m1", Role: "user", Content: "q"}, {ID: "m2", Role: "assistant", Content: "a"}}
	for key, who := range map[string]string{"thread-a": userA, "thread-b": userB} {
		if _, err := b.SaveConversationIf(ctx, flowID, key, who, 0, msgs); err != nil {
			t.Fatalf("SaveConversationIf %s: %v", key, err)
		}
	}
	// The release before's blind upsert (its SaveConversation, verbatim): no owner, so unowned.
	data, _ := json.Marshal(msgs[:1])
	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO conversations (flow_id, scope, messages, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (flow_id, scope) DO UPDATE SET
			messages   = EXCLUDED.messages,
			updated_at = EXCLUDED.updated_at
		WHERE conversations.messages IS DISTINCT FROM EXCLUDED.messages`, flowID, "flow", data, "2026-10-27T10:00:00Z"); err != nil {
		t.Fatalf("the release before's upsert: %v", err)
	}
	var (
		rowOwner sql.NullString
		count    sql.NullInt64
		created  sql.NullTime
	)
	if err := b.db.QueryRowContext(ctx, `SELECT owner_id, message_count, created_at FROM conversations WHERE flow_id = $1 AND scope = 'flow'`, flowID).
		Scan(&rowOwner, &count, &created); err != nil {
		t.Fatal(err)
	}
	if rowOwner.Valid || count.Int64 != 1 || !created.Valid {
		t.Errorf("the release before's row: owner %v, count %v, created %v; want unowned, 1, a creation time", rowOwner, count, created)
	}

	asUser := func(t *testing.T, user string, fn func(ctx context.Context, tx *sql.Tx)) {
		t.Helper()
		tx, err := b.BeginRLS(ctx, user)
		if err != nil {
			t.Fatalf("BeginRLS: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		fn(WithRLSTx(ctx, tx), tx)
	}
	asUser(t, userB, func(rctx context.Context, tx *sql.Tx) {
		scopes, err := rowsOfStrings(rctx, tx, `SELECT scope FROM conversations WHERE flow_id = $1 ORDER BY scope`, flowID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(scopes, ",") != "flow,thread-b" {
			t.Errorf("B sees %v under RLS, want B's own and the unowned one", scopes)
		}
		// Even the system view of the storage method sees only what the policy lets through.
		if got, _, err := b.LoadConversationVersioned(rctx, flowID, "thread-a", ""); err != nil || len(got) != 0 {
			t.Errorf("B's RLS read of A's thread = (%d, %v), want nothing", len(got), err)
		}
		res, err := tx.ExecContext(rctx, `UPDATE conversations SET messages = '[]' WHERE flow_id = $1 AND scope = 'thread-a'`, flowID)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("B changed A's thread under RLS (%d rows)", n)
		}
		if _, err := tx.ExecContext(rctx, `SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(rctx, `INSERT INTO conversations (flow_id, scope, messages, owner_id) VALUES ($1, 'forged', '[]', $2)`, flowID, userA); err == nil {
			t.Error("B created a row owned by A under RLS")
		}
		_, _ = tx.ExecContext(rctx, `ROLLBACK TO SAVEPOINT s`)
	})

	// The owner deletes the flow under their own user. Without the service's statement that the deletion is
	// authorised, the store refuses (a future caller that skips the FlowDelete check fails loudly instead of deleting
	// every user's threads as the system); with it, every user's threads go with the flow.
	tx, err := b.BeginRLS(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteFlow(WithRLSTx(ctx, tx), flowID); !errors.Is(err, interfaces.ErrFlowDeletionNotAuthorized) {
		t.Errorf("DeleteFlow under an RLS user without the authorisation = %v, want ErrFlowDeletionNotAuthorized", err)
	}
	_ = tx.Rollback()
	var left int
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM flows WHERE id = $1`, flowID).Scan(&left); err != nil || left != 1 {
		t.Fatalf("the refused deletion removed the flow (%d, %v)", left, err)
	}
	tx, err = b.BeginRLS(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteFlow(interfaces.WithFlowDeletionAuthorized(WithRLSTx(ctx, tx), flowID), flowID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("DeleteFlow: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM conversations WHERE flow_id = $1`, flowID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d conversations survived the flow's deletion under its owner's RLS user", left)
	}
}

// TestMigration0052_OwnerNeverReverts: an owned conversation never becomes unowned or another user's, whoever writes
// (the pool, a user under RLS); an unowned one may become its writer's (the claim), never another's.
func TestMigration0052_OwnerNeverReverts(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if v, _ := b.CurrentSchemaVersion(ctx); v < 52 {
		t.Skipf("DB at v%d; need v52", v)
	}
	for _, id := range []string{"m52r-a", "m52r-b"} {
		if err := b.CreateUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "hash", Role: "member"}); err != nil {
			t.Fatal(err)
		}
	}
	const flowID = "m52r-flow"
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: flowID, Name: "F", OwnerID: "m52r-a", Content: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SaveConversationIf(ctx, flowID, "owned", "m52r-a", 0, []interfaces.ChatMessage{{ID: "m", Role: "user", Content: "q"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(ctx, `INSERT INTO conversations (flow_id, scope, messages) VALUES ($1, 'shared', '[]')`, flowID); err != nil {
		t.Fatal(err)
	}
	owner := func(scope string) string {
		var o sql.NullString
		if err := b.db.QueryRowContext(ctx, `SELECT owner_id FROM conversations WHERE flow_id = $1 AND scope = $2`, flowID, scope).Scan(&o); err != nil {
			t.Fatal(err)
		}
		return o.String
	}
	for _, set := range []string{"NULL", "'m52r-b'"} {
		if _, err := b.db.ExecContext(ctx, `UPDATE conversations SET owner_id = `+set+` WHERE flow_id = $1 AND scope = 'owned'`, flowID); err == nil {
			t.Errorf("the pool set an owned conversation's owner to %s", set)
		}
	}
	tx, err := b.BeginRLS(ctx, "m52r-a")
	if err != nil {
		t.Fatal(err)
	}
	rctx := WithRLSTx(ctx, tx)
	if _, err := tx.ExecContext(rctx, `UPDATE conversations SET owner_id = NULL WHERE flow_id = $1 AND scope = 'owned'`, flowID); err == nil {
		t.Error("A un-owned their conversation under RLS (it would become shared)")
	}
	_ = tx.Rollback()
	tx, err = b.BeginRLS(ctx, "m52r-a")
	if err != nil {
		t.Fatal(err)
	}
	rctx = WithRLSTx(ctx, tx)
	if _, err := tx.ExecContext(rctx, `UPDATE conversations SET owner_id = 'm52r-b' WHERE flow_id = $1 AND scope = 'shared'`, flowID); err == nil {
		t.Error("A gave an unowned conversation to B under RLS")
	}
	_ = tx.Rollback()
	tx, err = b.BeginRLS(ctx, "m52r-a")
	if err != nil {
		t.Fatal(err)
	}
	rctx = WithRLSTx(ctx, tx)
	if _, err := tx.ExecContext(rctx, `UPDATE conversations SET owner_id = 'm52r-a' WHERE flow_id = $1 AND scope = 'shared'`, flowID); err != nil {
		t.Errorf("A's claim of an unowned conversation under RLS: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if o := owner("owned"); o != "m52r-a" {
		t.Errorf("owned conversation's owner = %q, want m52r-a", o)
	}
	if o := owner("shared"); o != "m52r-a" {
		t.Errorf("claimed conversation's owner = %q, want m52r-a", o)
	}
}
