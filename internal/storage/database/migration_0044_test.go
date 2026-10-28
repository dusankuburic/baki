package database

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// TestMigration0044_Shape pins the v44 step without a database: expand-compatible, a metadata-only column (a
// constant default, so no table rewrite), a trigger on INSERT and UPDATE, and a down step that removes all four
// objects it created.
func TestMigration0044_Shape(t *testing.T) {
	m, ok := migrationByVersion(44)
	if !ok {
		t.Fatal("migration v44 not registered")
	}
	if m.name != "conversation_version" {
		t.Errorf("v44 name = %q, want conversation_version", m.name)
	}
	if m.compat != compatExpand {
		t.Errorf("v44 compat = %v, want expand (the release before ignores the column and its blind saves still work)", m.compat)
	}
	up := oneLine(m.sql)
	for _, want := range []string{
		"CREATE SEQUENCE IF NOT EXISTS conversation_version_seq;",
		// A volatile default (nextval) would rewrite the table; the constant does not.
		"ALTER TABLE conversations ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 0;",
		"BEFORE INSERT OR UPDATE ON conversations",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("v44 lacks %q", want)
		}
	}
	for _, forbidden := range []string{"DEFAULT NEXTVAL", "CREATE INDEX", "CONCURRENTLY"} {
		if strings.Contains(strings.ToUpper(m.sql), forbidden) {
			t.Errorf("v44 must not contain %s (it would rewrite or lock the table)", forbidden)
		}
	}
	down := oneLine(m.downSQL)
	for _, want := range []string{
		"DROP TRIGGER IF EXISTS conversations_version ON conversations;",
		"DROP FUNCTION IF EXISTS conversations_bump_version();",
		"ALTER TABLE conversations DROP COLUMN IF EXISTS version;",
		"DROP SEQUENCE IF EXISTS conversation_version_seq;",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("v44 down lacks %q", want)
		}
	}
}

// TestMigration0044_TriggerBumpsOnAnySQL is what makes the version safe in a rolling deploy: the release before
// writes conversations with SQL that names no version column, and every such change must still move the version.
func TestMigration0044_TriggerBumpsOnAnySQL(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if v, _ := b.CurrentSchemaVersion(ctx); v < 44 {
		t.Skipf("DB at v%d; need v44", v)
	}
	version := func(flow string) (v int64) {
		t.Helper()
		if err := b.db.QueryRowContext(ctx, `SELECT version FROM conversations WHERE flow_id = $1 AND scope = 's'`, flow).Scan(&v); err != nil {
			t.Fatalf("read version: %v", err)
		}
		return v
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := b.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// An insert that names no version (the old binary's upsert) gets one.
	exec(`INSERT INTO conversations (flow_id, scope, messages) VALUES ('f44', 's', '[{"id":"m1"}]')`)
	v1 := version("f44")
	if v1 <= 0 {
		t.Fatalf("a row inserted without a version has version %d, want a positive one", v1)
	}
	// An insert that tries to name one is overridden: a writer cannot pick a number another conversation had.
	exec(`INSERT INTO conversations (flow_id, scope, messages, version) VALUES ('f44b', 's', '[]', 999999999)`)
	if v := version("f44b"); v == 999999999 {
		t.Errorf("an inserted version was kept; the trigger must assign it")
	}

	// The old binary's upsert: ON CONFLICT DO UPDATE with new messages bumps, with the same messages does not.
	upsert := `INSERT INTO conversations (flow_id, scope, messages, updated_at) VALUES ('f44', 's', $1, now())
		ON CONFLICT (flow_id, scope) DO UPDATE SET messages = EXCLUDED.messages, updated_at = EXCLUDED.updated_at
		WHERE conversations.messages IS DISTINCT FROM EXCLUDED.messages`
	exec(upsert, `[{"id":"m1"},{"id":"m2"}]`)
	v2 := version("f44")
	if v2 <= v1 {
		t.Errorf("a blind upsert with new messages left the version %d -> %d, want it to move", v1, v2)
	}
	exec(upsert, `[{"id":"m1"},{"id":"m2"}]`)
	if v := version("f44"); v != v2 {
		t.Errorf("the same messages moved the version %d -> %d", v2, v)
	}
	// An UPDATE that leaves the messages alone does not move it either; one that changes them does.
	exec(`UPDATE conversations SET updated_at = now() WHERE flow_id = 'f44'`)
	if v := version("f44"); v != v2 {
		t.Errorf("an update of updated_at moved the version %d -> %d", v2, v)
	}
	exec(`UPDATE conversations SET messages = '[]' WHERE flow_id = 'f44'`)
	if v := version("f44"); v <= v2 {
		t.Errorf("an update of messages left the version at %d (was %d)", v, v2)
	}

	// A legacy row (version 0, as every row is when the migration runs) takes a compare-and-swap at 0 once.
	exec(`UPDATE conversations SET version = 0 WHERE flow_id = 'f44'`)
	if v := version("f44"); v != 0 {
		t.Fatalf("could not stage a legacy row: version %d", v)
	}
	msgs := []interfaces.ChatMessage{{ID: "m9", Role: "user", Content: "after the migration"}}
	v3, err := b.SaveConversationIf(ctx, "f44", "s", "", 0, msgs)
	if err != nil || v3 <= 0 {
		t.Fatalf("SaveConversationIf(0) of a legacy row = (%d, %v), want a positive version", v3, err)
	}
	if _, err := b.SaveConversationIf(ctx, "f44", "s", "", 0, msgs); err == nil {
		t.Error("a second SaveConversationIf(0) succeeded; the legacy row has a version now")
	}
}

// TestMigration0044_DownRemovesEverything rolls the step back on a private database: the column, the trigger, its
// function and the sequence are gone, and the old statements still run. The next boot re-applies the step.
func TestMigration0044_DownRemovesEverything(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if v, _ := b.CurrentSchemaVersion(ctx); v < 44 {
		t.Skipf("DB at v%d; need v44", v)
	}
	if _, err := b.MigrateDown(ctx, 43); err != nil {
		t.Fatalf("MigrateDown(43): %v", err)
	}
	count := func(q string) (n int) {
		t.Helper()
		if err := b.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	for what, q := range map[string]string{
		"column":   `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'conversations' AND column_name = 'version'`,
		"trigger":  `SELECT count(*) FROM pg_trigger WHERE tgname = 'conversations_version'`,
		"function": `SELECT count(*) FROM pg_proc WHERE proname = 'conversations_bump_version'`,
		"sequence": `SELECT count(*) FROM pg_class WHERE relname = 'conversation_version_seq'`,
	} {
		if n := count(q); n != 0 {
			t.Errorf("after the down step the %s is still there", what)
		}
	}
	// The release before: its upsert names no version.
	if _, err := b.db.ExecContext(ctx, `INSERT INTO conversations (flow_id, scope, messages, updated_at) VALUES ('f44d', 's', '[]', now())
		ON CONFLICT (flow_id, scope) DO UPDATE SET messages = EXCLUDED.messages`); err != nil {
		t.Errorf("the previous release's upsert after the down step: %v", err)
	}
}
