package database

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// stepStatements splits a step's SQL into its top-level statements, in order: comment lines outside a dollar-quoted
// body are dropped, and a statement ends at a line ending in ';' outside one (a DO block is one statement).
func stepStatements(sql string) []string {
	dollar := regexp.MustCompile(`\$[A-Za-z0-9_]*\$`)
	var out []string
	var cur strings.Builder
	inBody := false
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inBody && (trimmed == "" || strings.HasPrefix(trimmed, "--")) {
			continue
		}
		if len(dollar.FindAllString(line, -1))%2 == 1 {
			inBody = !inBody
		}
		cur.WriteString(line)
		cur.WriteString("\n")
		if !inBody && strings.HasSuffix(trimmed, ";") {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// TestMigration0048_BlocksWritesOnlyInItsTail: 0048 runs at boot while the release before still serves, and a step
// holds its locks until it commits. So the step never reads a table that grows with use (sessions, knowledge chunks,
// flow versions, triage, alerts): not under a lock, where the read holds writers, and not before one either, since a
// row lock taken there on a row a request reaches deadlocks with that request once the step waits for the table (its
// orphans are SchemaUpkeep's). From its first lock that writers wait for (SHARE or stronger) on, the statement taking
// it included, the step reads only tables of a row per policy, share link, invitation, upload or account: the foreign
// keys are added NOT VALID and validated after boot, the actor indexes built concurrently after boot (SchemaUpkeep).
// The tables only those indexes touch are never locked against writes.
// Once it locks what every sign-in writes (refresh_tokens, users) it locks and reads nothing else. Each statement
// runs in one transaction, as the runner runs the step; after each, pg_locks says what the step holds and
// pg_stat_xact_user_tables what it read. TestMigration0048_SignInsNeverWaitOnItsLockWaits checks its lock waits.
func TestMigration0048_BlocksWritesOnlyInItsTail(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if rolled, err := b.MigrateDown(ctx, 47); err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 48 {
		t.Fatalf("MigrateDown(47) = %v, %v", rolled, err)
	}
	// A row in every table the step reads, so a read shows in the counters.
	for _, s := range []string{
		`INSERT INTO users (id, email, password, role) VALUES ('l48-u', 'l48-u@example.com', 'x', 'member')`,
		`INSERT INTO organisations (id, name, owner_id) VALUES ('l48-org', 'Org', 'l48-u')`,
		`INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ('l48-rt', 'l48-u', NOW() + INTERVAL '1 hour')`,
		`INSERT INTO knowledge_documents (id, org_id, filename) VALUES ('l48-kd', 'l48-org', 'a.md')`,
		`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES ('l48-kc', 'l48-kd', 'l48-org', 'x', '[]')`,
		`INSERT INTO flows (id, name, owner_id) VALUES ('l48-flow', 'F', 'l48-u')`,
		`INSERT INTO flow_versions (id, flow_id, version, created_by) VALUES ('l48-v', 'l48-flow', 1, 'l48-u')`,
		`INSERT INTO finding_comments (id, flow_id, finding_key, author_id, body) VALUES ('l48-c', 'l48-flow', 'r:1', 'l48-u', 'x')`,
		`INSERT INTO finding_status (flow_id, finding_key, status, assignee_id, updated_by) VALUES ('l48-flow', 'r:1', 'acknowledged', 'l48-u', 'l48-u')`,
		`INSERT INTO gov_alerts (id, flow_id, type, dismissed_at) VALUES ('l48-a', 'l48-flow', 'health', NOW())`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}

	// What the tail may read under its locks: a row per organisation, channel, policy, share link, invitation, uploaded
	// file and account. Any other table grows with use, and reading it takes time proportional to the deployment's
	// history.
	small := []string{"organisations", "org_channels", "policies", "share_tokens", "org_invites", "knowledge_documents", "users"}
	// What every sign-in writes: locked last, with nothing but their own statements after them.
	hot := []string{"refresh_tokens", "users"}
	// Tables the step has no constraint on: a flow save, a comment, a triage change or an alert never waits for it.
	neverLocked := []string{"flow_versions", "finding_comments", "finding_status", "gov_alerts"}

	m, ok := migrationByVersion(48)
	if !ok {
		t.Fatal("migration v48 not registered")
	}
	stmts := stepStatements(m.sql)
	if len(stmts) < 5 {
		t.Fatalf("split v48 into %d statements; the splitter is off", len(stmts))
	}
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		t.Fatal(err)
	}
	reads := func() map[string]int64 {
		t.Helper()
		rows, err := tx.QueryContext(ctx, `SELECT relname, seq_scan + COALESCE(idx_scan, 0) FROM pg_stat_xact_user_tables`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var name string
			var n int64
			if err := rows.Scan(&name, &n); err != nil {
				t.Fatal(err)
			}
			out[name] = n
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// writeBlocking is every table the transaction holds a lock on that INSERT, UPDATE and DELETE wait for.
	writeBlocking := func() map[string]string {
		t.Helper()
		rows, err := tx.QueryContext(ctx, `SELECT c.relname, l.mode FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
			WHERE l.pid = pg_backend_pid() AND l.locktype = 'relation' AND c.relkind = 'r' AND c.relnamespace = current_schema()::regnamespace
			  AND l.mode IN ('ShareLock', 'ShareRowExclusiveLock', 'ExclusiveLock', 'AccessExclusiveLock')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, mode string
			if err := rows.Scan(&name, &mode); err != nil {
				t.Fatal(err)
			}
			out[name] += mode + " "
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	read, reported := map[string]bool{}, map[string]bool{}
	held := map[string]string{}
	hotHeld := false
	before := reads()
	for i, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			t.Fatalf("statement %d: %v\n%s", i+1, err, s)
		}
		after := reads()
		heldAfter := writeBlocking()
		for table, n := range after {
			if n <= before[table] {
				continue
			}
			read[table] = true
			// The locks held after the statement count too: a statement that takes the step's first such lock and
			// reads in the same breath (a validating key, a plain index build) holds writers for its whole read.
			if locked := slices.Sorted(maps.Keys(heldAfter)); len(locked) > 0 && !slices.Contains(small, table) {
				t.Errorf("statement %d reads %s while the step blocks writes to %v until it commits:\n%s", i+1, table, locked, s)
			}
			if hotHeld && !slices.Contains(hot, table) {
				t.Errorf("statement %d reads %s after the step locked refresh_tokens and users:\n%s", i+1, table, s)
			}
		}
		for table := range heldAfter {
			if _, ok := held[table]; hotHeld && !ok && !slices.Contains(hot, table) {
				t.Errorf("statement %d locks %s after the step locked refresh_tokens and users:\n%s", i+1, table, s)
			}
		}
		held = heldAfter
		hotHeld = hotHeld || held["refresh_tokens"] != "" || held["users"] != ""
		for _, table := range neverLocked {
			if mode, ok := held[table]; ok && !reported[table] {
				reported[table] = true
				t.Errorf("statement %d locks %s against writes (%s) until the step commits:\n%s", i+1, table, strings.TrimSpace(mode), s)
			}
		}
		before = after
	}
	// The sessions and the chunks, the growing tables the step adds keys on, are read nowhere in it: their orphans
	// (a session of an erased account, a chunk filed under another organisation) are deleted by SchemaUpkeep, since
	// a refresh or a re-index of the release before can reach them. The check sees what it is about: the foreign
	// keys' locks are taken, at the end.
	for _, table := range []string{"refresh_tokens", "knowledge_chunks"} {
		if read[table] {
			t.Errorf("the step reads %s, which grows with use; its orphans are SchemaUpkeep's", table)
		}
	}
	if held["refresh_tokens"] == "" || held["users"] == "" {
		t.Errorf("at its end the step blocks writes to %v; want refresh_tokens and users among them (the session foreign key)", held)
	}
}
