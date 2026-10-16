package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// v48Constraints are the foreign keys migration 0048 adds, by table.
var v48Constraints = map[string]string{
	"knowledge_documents": "knowledge_documents_org_id_fkey",
	"knowledge_chunks":    "knowledge_chunks_doc_id_org_id_fkey",
	"policies":            "policies_org_id_fkey",
	"org_channels":        "org_channels_org_id_fkey",
	"org_custom_rules":    "org_custom_rules_org_id_fkey",
	"refresh_tokens":      "refresh_tokens_user_id_fkey",
}

// v48StepIndexes are the unique indexes migration 0048 builds itself; v48UpkeepIndexes are the alert inbox's and
// account erasure's, which SchemaUpkeep builds concurrently after boot (plans_indexes_0048_test.go checks their
// plans).
var (
	v48StepIndexes   = []string{"users_lower_email_key", "knowledge_documents_id_org_id_key", "org_invites_token_hash_key"}
	v48UpkeepIndexes = []string{"gov_alerts_dismissed_flow_idx", "flow_versions_created_by_idx", "finding_comments_author_idx",
		"finding_status_updated_by_idx", "finding_status_assignee_idx"}
)

// TestMigration0048_Shape pins the step without a database: expand, every foreign key added NOT VALID and left to
// SchemaUpkeep to validate, the refusals before any change, no foreign key the release before would violate
// (org_members.user_id, conversations.flow_id) and no policy id CHECK yet, the actor indexes not in the step but in
// SchemaUpkeep's concurrent builds, and a down step that removes it all.
func TestMigration0048_Shape(t *testing.T) {
	m, ok := migrationByVersion(48)
	if !ok {
		t.Fatal("migration v48 not registered")
	}
	if m.name != "integrity_constraints" || m.compat != compatExpand {
		t.Errorf("v48 = %q (%v), want integrity_constraints, expand (every constraint accepts what the release before writes)", m.name, m.compat)
	}
	up := oneLine(m.sql)
	stmts := stepStatements(m.sql)
	upkept := map[string]pendingConstraint{}
	for _, c := range schemaUpkeepConstraints {
		upkept[c.name] = c
	}
	for table, name := range v48Constraints {
		found := false
		for _, s := range stmts {
			s = oneLine(s)
			if strings.Contains(s, "ADD CONSTRAINT "+name+" FOREIGN KEY") {
				found = strings.HasPrefix(s, "ALTER TABLE "+table+" ") && strings.Contains(s, "ON DELETE CASCADE NOT VALID")
			}
		}
		if !found {
			t.Errorf("v48 does not add %s on %s ON DELETE CASCADE NOT VALID", name, table)
		}
		if c, ok := upkept[name]; !ok || c.since != 48 || c.table != table || !strings.HasPrefix(c.orphans, "DELETE FROM "+table+" ") {
			t.Errorf("SchemaUpkeep does not validate %s (with its orphan delete): %+v", name, c)
		}
	}
	for _, want := range []string{
		"FOREIGN KEY (doc_id, org_id) REFERENCES knowledge_documents (id, org_id)",
		"CREATE UNIQUE INDEX IF NOT EXISTS users_lower_email_key ON users (lower(email));",
		"CREATE UNIQUE INDEX IF NOT EXISTS knowledge_documents_id_org_id_key ON knowledge_documents (id, org_id);",
		"CREATE UNIQUE INDEX IF NOT EXISTS org_invites_token_hash_key ON org_invites (token_hash);",
		"DROP INDEX IF EXISTS org_invites_token_hash_idx;",
		"ALTER TABLE share_tokens ALTER COLUMN created_by SET DEFAULT '', ALTER COLUMN created_by SET NOT NULL;",
		"ALTER TABLE policies ALTER COLUMN description SET NOT NULL, ADD CONSTRAINT policies_org_id_fkey",
		"UPDATE policies SET id = gen_random_uuid()::text WHERE id = '';",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("v48 lacks %q", want)
		}
	}
	// The actor indexes: the same statements as the plan tests', built CONCURRENTLY after boot, never in the step.
	var built []string
	for _, ix := range schemaUpkeepIndexes {
		if ix.since != 48 {
			continue
		}
		built = append(built, ix.name)
		if want := strings.Replace(indexes0048[ix.name], "CREATE INDEX IF NOT EXISTS", "CREATE INDEX CONCURRENTLY IF NOT EXISTS", 1); ix.ddl != want {
			t.Errorf("SchemaUpkeep builds %s as %q, want %q", ix.name, ix.ddl, want)
		}
		if strings.Contains(up, ix.name) {
			t.Errorf("v48 itself names %s; its build blocks writes to the table until the step commits", ix.name)
		}
	}
	if !slices.Equal(built, v48UpkeepIndexes) {
		t.Errorf("SchemaUpkeep builds %v for v48, want %v (plans_indexes_0048_test.go names them)", built, v48UpkeepIndexes)
	}
	// The refusals run before the first change, so a refused step has nothing to roll back but itself.
	if r, d := strings.Index(up, "RAISE EXCEPTION"), strings.Index(up, "DELETE FROM"); r < 0 || d < 0 || r > d {
		t.Errorf("the duplicate checks (at %d) must come before the first delete (at %d)", r, d)
	}
	upper := strings.ToUpper(strings.Join(stepStatements(m.sql), " ")) // the statements, not the comments
	for _, forbidden := range []string{"CONCURRENTLY", "VALIDATE CONSTRAINT", "ALTER TABLE ORG_MEMBERS", "ALTER TABLE CONVERSATIONS", "REFERENCES FLOWS", "CHECK (ID <> '')", "USERS_EMAIL_KEY;"} {
		if strings.Contains(upper, forbidden) {
			t.Errorf("v48 must not contain %s", forbidden)
		}
	}
	down := oneLine(m.downSQL)
	for table, name := range v48Constraints {
		if !strings.Contains(down, "ALTER TABLE "+table) || !strings.Contains(down, "DROP CONSTRAINT IF EXISTS "+name+";") {
			t.Errorf("v48 down does not drop %s", name)
		}
	}
	for _, want := range append([]string{
		"CREATE INDEX IF NOT EXISTS org_invites_token_hash_idx ON org_invites (token_hash);",
		"ALTER TABLE policies ALTER COLUMN description DROP NOT NULL;",
		"ALTER TABLE share_tokens ALTER COLUMN created_by DROP NOT NULL, ALTER COLUMN created_by DROP DEFAULT;",
	}, func() (out []string) {
		for _, ix := range append(slices.Clone(v48StepIndexes), v48UpkeepIndexes...) {
			out = append(out, "DROP INDEX IF EXISTS "+ix+";")
		}
		return out
	}()...) {
		if !strings.Contains(down, want) {
			t.Errorf("v48 down lacks %q", want)
		}
	}
}

// v48State is what of migration 0048 the schema has: its foreign keys (and how many are validated), its own indexes
// and SchemaUpkeep's, whether the v47 token index exists and whether the two columns are NOT NULL.
type v48State struct {
	fks, validated, stepIndexes, upkeepIndexes int
	oldIndex, notNull                          bool
}

func v48Shape(t *testing.T, ctx context.Context, b *PostgresStorageBackend) v48State {
	t.Helper()
	var s v48State
	for table, name := range v48Constraints {
		var valid bool
		err := b.db.QueryRowContext(ctx, `SELECT convalidated FROM pg_constraint WHERE conrelid = $1::regclass AND conname = $2`, table, name).Scan(&valid)
		if err == nil {
			s.fks++
			if valid {
				s.validated++
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
	}
	count := func(names []string) (n int) {
		for _, ix := range names {
			var ok bool
			if err := b.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
				WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace AND i.indisvalid)`, ix).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				n++
			}
		}
		return n
	}
	s.stepIndexes, s.upkeepIndexes = count(v48StepIndexes), count(v48UpkeepIndexes)
	if err := b.db.QueryRowContext(ctx, `SELECT to_regclass('org_invites_token_hash_idx') IS NOT NULL`).Scan(&s.oldIndex); err != nil {
		t.Fatal(err)
	}
	if err := b.db.QueryRowContext(ctx, `SELECT bool_and(is_nullable = 'NO') FROM information_schema.columns
		WHERE table_schema = current_schema() AND (table_name, column_name) IN (('share_tokens', 'created_by'), ('policies', 'description'))`).Scan(&s.notNull); err != nil {
		t.Fatal(err)
	}
	return s
}

// upkept runs SchemaUpkeep on b and fails the test when it did not finish.
func upkept(t *testing.T, b *PostgresStorageBackend) {
	t.Helper()
	if err := b.runSchemaUpkeep(context.Background()); err != nil {
		t.Fatalf("schema upkeep: %v", err)
	}
}

// TestMigration0048_UpDownUp: at v48 every foreign key is there, not yet validated, with the step's indexes and NOT
// NULLs; SchemaUpkeep validates the keys and builds the actor indexes; a rollback removes all of it and brings the
// v47 token index back; a re-apply restores the v48 shape, and the upkeep its validation and indexes.
func TestMigration0048_UpDownUp(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	booted := v48State{fks: len(v48Constraints), stepIndexes: len(v48StepIndexes), notNull: true}
	done := booted
	done.validated, done.upkeepIndexes = len(v48Constraints), len(v48UpkeepIndexes)
	if s := v48Shape(t, ctx, b); s != booted {
		t.Fatalf("at v48 after boot: %+v, want %+v", s, booted)
	}
	upkept(t, b)
	if s := v48Shape(t, ctx, b); s != done {
		t.Fatalf("at v48 after the upkeep: %+v, want %+v", s, done)
	}
	rolled, err := b.MigrateDown(ctx, 47)
	if err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 48 {
		t.Fatalf("MigrateDown(47) = %v, %v; want a list ending in 48", rolled, err)
	}
	if s := v48Shape(t, ctx, b); s != (v48State{oldIndex: true}) {
		t.Fatalf("after the rollback: %+v; want nothing of v48 and the v47 token index", s)
	}
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("reconnect (re-apply v48): %v", err)
	}
	t.Cleanup(func() { b2.Close() })
	if s := v48Shape(t, ctx, b2); s != booted {
		t.Fatalf("after the re-apply: %+v, want %+v", s, booted)
	}
	upkept(t, b2)
	if s := v48Shape(t, ctx, b2); s != done {
		t.Fatalf("after the re-apply's upkeep: %+v, want %+v", s, done)
	}
}

// TestMigration0048_DeletesOrphansAndRefusesDuplicates seeds, at v47, what the release before could leave behind and
// what the step must not guess about. Accounts whose emails differ only in case, then invites sharing a token, make
// the step fail with the ids (never the address) and change nothing; once the operator resolves them it deletes the
// orphans of its small tables, keeps everything else, gives the id-less policy an id and fills the NULLs; the
// orphans of the tables that grow with use wait for SchemaUpkeep, which deletes them before it validates their keys.
func TestMigration0048_DeletesOrphansAndRefusesDuplicates(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if rolled, err := b.MigrateDown(ctx, 47); err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 48 {
		t.Fatalf("MigrateDown(47) = %v, %v", rolled, err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := b.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, email, password, role) VALUES
		('m48-owner', 'm48-owner@example.com', 'x', 'member'),
		('m48-dup-a', 'M48-Dup@Example.com', 'x', 'member'),
		('m48-dup-b', 'm48-dup@example.com', 'x', 'member')`)
	exec(`INSERT INTO organisations (id, name, owner_id) VALUES ('m48-org', 'Org', 'm48-owner'), ('m48-org2', 'Org 2', 'm48-owner')`)
	// local mode on a database: the user 'local' has no users row; a chat about a file that was never a library flow.
	exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ('m48-org', 'local', 'admin')`)
	exec(`INSERT INTO conversations (flow_id, scope, messages) VALUES ('m48-unsaved-file', 'openai', '[]')`)
	exec(`INSERT INTO flows (id, name, owner_id) VALUES ('m48-flow', 'F', 'm48-owner')`)
	exec(`INSERT INTO share_tokens (id, flow_id, token_hash, created_by) VALUES ('m48-share', 'm48-flow', 'm48-share-hash', NULL)`)
	exec(`INSERT INTO policies (id, org_id, name, description) VALUES
		('m48-pol-gone', 'm48-gone', 'Orphan', ''), ('m48-pol', 'm48-org', 'Kept', NULL), ('', 'm48-org', 'Id-less', 'd')`)
	exec(`INSERT INTO org_channels (id, org_id, kind, url) VALUES ('m48-ch-gone', 'm48-gone', 'webhook', 'u'), ('m48-ch', 'm48-org', 'webhook', 'u')`)
	exec(`INSERT INTO org_custom_rules (id, org_id, rule_id, config) VALUES ('m48-cr-gone', 'm48-gone', 'r', '{}'), ('m48-cr', 'm48-org', 'r', '{}')`)
	exec(`INSERT INTO knowledge_documents (id, org_id, filename) VALUES ('m48-kd-gone', 'm48-gone', 'a.md'), ('m48-kd', 'm48-org', 'b.md')`)
	exec(`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES
		('m48-kc-gone', 'm48-kd-gone', 'm48-gone', 'x', '[]'),
		('m48-kc-elsewhere', 'm48-kd', 'm48-org2', 'x', '[]'),
		('m48-kc', 'm48-kd', 'm48-org', 'x', '[]')`)
	exec(`INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ('m48-rt-gone', 'm48-erased', NOW() + INTERVAL '1 hour'), ('m48-rt', 'm48-owner', NOW() + INTERVAL '1 hour'),
		('m48-rt-gone-expired', 'm48-erased', NOW() - INTERVAL '1 hour')`)
	in := time.Now().Add(time.Hour)
	exec(`INSERT INTO org_invites (id, org_id, email, role, invited_by, token_hash, expires_at) VALUES
		('m48-inv-a', 'm48-org', 'a@example.com', 'member', 'm48-owner', 'm48-same-hash', $1),
		('m48-inv-b', 'm48-org', 'b@example.com', 'member', 'm48-owner', 'm48-same-hash', $1)`, in)

	count := func(q string) int {
		t.Helper()
		var n int
		if err := b.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	// The step's own: rows of the tables of a row per policy, channel and rule, which no request of the release
	// before reaches once their organisation is gone.
	stepOrphans := `SELECT (SELECT count(*) FROM policies WHERE id = 'm48-pol-gone') + (SELECT count(*) FROM org_channels WHERE id = 'm48-ch-gone')
		+ (SELECT count(*) FROM org_custom_rules WHERE id = 'm48-cr-gone')`
	// The upkeep's: the tables that grow with use, which the step neither reads nor row-locks (a re-index reaches the
	// misfiled chunk, a refresh the live session); the expired session is the retention sweep's too.
	upkeepOrphans := `SELECT (SELECT count(*) FROM knowledge_documents WHERE id = 'm48-kd-gone')
		+ (SELECT count(*) FROM knowledge_chunks WHERE id IN ('m48-kc-gone', 'm48-kc-elsewhere'))
		+ (SELECT count(*) FROM refresh_tokens WHERE jti IN ('m48-rt-gone', 'm48-rt-gone-expired'))`
	refused := func(wantIDs ...string) {
		t.Helper()
		b2, err := New(ctx, DefaultConfig(dsn))
		if err == nil {
			b2.Close()
			t.Fatalf("the step applied over duplicates %v", wantIDs)
		}
		for _, id := range wantIDs {
			if !strings.Contains(err.Error(), id) {
				t.Errorf("the refusal does not name %s: %v", id, err)
			}
		}
		if strings.Contains(strings.ToLower(err.Error()), "m48-dup@example.com") {
			t.Errorf("the refusal logs an email address: %v", err)
		}
		if v, _ := b.CurrentSchemaVersion(ctx); v != 47 {
			t.Errorf("after the refusal the schema is at v%d, want 47", v)
		}
		if n := count(stepOrphans) + count(upkeepOrphans); n != 8 {
			t.Errorf("after the refusal %d of the 8 orphans are left, want all (nothing changed)", n)
		}
	}
	refused("m48-dup-a", "m48-dup-b")
	// operations.md's recipe (TestMigration0048_DuplicateEmailRecipe follows it in full): find the groups, keep one
	// account, rename the others and take every way in from them.
	exec(`INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ('m48-rt-dup', 'm48-dup-b', NOW() + INTERVAL '1 hour')`)
	var address, group string
	if err := b.db.QueryRowContext(ctx, duplicateEmailGroupsSQL).Scan(&address, &group); err != nil || group != "m48-dup-a, m48-dup-b" {
		t.Fatalf("the recipe's group query = %q, %v", group, err)
	}
	resolveDuplicateEmail(t, b.db, "m48-dup-a", "m48-dup-b")
	if n := count(`SELECT count(*) FROM refresh_tokens WHERE user_id = 'm48-dup-b' AND NOT revoked`); n != 0 {
		t.Errorf("%d sessions of the renamed account still live", n)
	}
	refused("m48-inv-a", "m48-inv-b")
	exec(`DELETE FROM org_invites WHERE id IN ('m48-inv-b')`)

	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("the step once the duplicates are resolved: %v", err)
	}
	t.Cleanup(func() { b2.Close() })
	if v, _ := b2.CurrentSchemaVersion(ctx); v < 48 {
		t.Fatalf("schema at v%d, want 48", v)
	}
	if n := count(stepOrphans); n != 0 {
		t.Errorf("%d of the step's orphans survived it", n)
	}
	if n := count(upkeepOrphans); n != 5 {
		t.Errorf("the step left %d of the 5 orphans of the growing tables; want all (they are the upkeep's)", n)
	}
	kept := `SELECT (SELECT count(*) FROM policies WHERE id = 'm48-pol') + (SELECT count(*) FROM org_channels WHERE id = 'm48-ch')
		+ (SELECT count(*) FROM org_custom_rules WHERE id = 'm48-cr') + (SELECT count(*) FROM knowledge_documents WHERE id = 'm48-kd')
		+ (SELECT count(*) FROM knowledge_chunks WHERE id = 'm48-kc') + (SELECT count(*) FROM refresh_tokens WHERE jti = 'm48-rt')
		+ (SELECT count(*) FROM org_members WHERE user_id = 'local') + (SELECT count(*) FROM conversations WHERE flow_id = 'm48-unsaved-file')
		+ (SELECT count(*) FROM org_invites WHERE id = 'm48-inv-a')`
	if n := count(kept); n != 9 {
		t.Errorf("%d of the 9 rows the step must keep are left", n)
	}
	var id, desc string
	if err := b2.db.QueryRowContext(ctx, `SELECT id, description FROM policies WHERE org_id = 'm48-org' AND name = 'Id-less'`).Scan(&id, &desc); err != nil || id == "" || desc != "d" {
		t.Errorf("the id-less policy = (%q, %q), %v; want it kept under a generated id", id, desc, err)
	}
	if err := b2.db.QueryRowContext(ctx, `SELECT description FROM policies WHERE id = 'm48-pol'`).Scan(&desc); err != nil || desc != "" {
		t.Errorf("the NULL description = %q, %v; want ''", desc, err)
	}
	var by string
	if err := b2.db.QueryRowContext(ctx, `SELECT created_by FROM share_tokens WHERE id = 'm48-share'`).Scan(&by); err != nil || by != "" {
		t.Errorf("the NULL creator = %q, %v; want ''", by, err)
	}
	// The upkeep deletes what each key refuses and validates it; the kept rows satisfy every key.
	upkept(t, b2)
	if n := count(upkeepOrphans); n != 0 {
		t.Errorf("%d orphan(s) of the growing tables survived the upkeep", n)
	}
	if n := count(kept); n != 9 {
		t.Errorf("%d of the 9 rows the upkeep must keep are left", n)
	}
	if s := v48Shape(t, ctx, b2); s.validated != len(v48Constraints) {
		t.Errorf("after the upkeep %d of the %d foreign keys are validated", s.validated, len(v48Constraints))
	}
}

// TestMigration0048_ThePreviousReleaseKeepsWriting runs the v47 release's own statements on v48, the image rollback
// an expand step promises: each constraint accepts what that release writes, the cascades take the rows the
// org-delete and erasure steps would have deleted by hand, and the rows the step leaves unconstrained stay writable.
func TestMigration0048_ThePreviousReleaseKeepsWriting(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	upkept(t, b) // the shape the release before meets once the upkeep has run: keys validated, actor indexes built
	exec := func(what, q string, args ...any) {
		t.Helper()
		if _, err := b.db.ExecContext(ctx, q, args...); err != nil {
			t.Errorf("the release before's %s on v48: %v", what, err)
		}
	}
	exec("registration", `INSERT INTO users (id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at)
		VALUES ('p48-u', 'p48-u@example.com', 'x', 'member', false, 0, NULL, '', '', NOW(), NOW())`)
	exec("organisation", `INSERT INTO organisations (id, name, owner_id) VALUES ('p48-org', 'Org', 'p48-u')`)
	exec("local-mode membership", `INSERT INTO org_members (org_id, user_id, role, joined_at) VALUES ('p48-org', 'local', 'admin', NOW())`)
	exec("session", `INSERT INTO refresh_tokens (jti, user_id, expires_at, user_agent, ip) VALUES ('p48-rt', 'p48-u', NOW() + INTERVAL '1 hour', '', '')
		ON CONFLICT (jti) DO NOTHING`)
	// The policy panel sends no id; that release stores it under ''.
	exec("id-less policy", `INSERT INTO policies (id, org_id, name, description, rules, gate_severity, waivers, max_errors, max_warnings, max_info, created_at, updated_at)
		VALUES ('', 'p48-org', 'Gate', '', '[]', 'warning', NULL, NULL, NULL, NULL, NOW(), NOW())
		ON CONFLICT (id, org_id) DO UPDATE SET name = EXCLUDED.name, updated_at = NOW()`)
	exec("channel", `INSERT INTO org_channels (id, org_id, name, kind, url, secret, enabled, created_at, config, secrets, events)
		VALUES ('p48-ch', 'p48-org', 'ops', 'webhook', 'u', '', true, NOW(), '{}', '', NULL)`)
	exec("custom rule", `INSERT INTO org_custom_rules (id, org_id, rule_id, config, enabled, created_at, updated_at) VALUES ('p48-cr', 'p48-org', 'r', '{}', true, NOW(), NOW())`)
	exec("knowledge document", `INSERT INTO knowledge_documents (id, org_id, filename, chunker_version) VALUES ('p48-kd', 'p48-org', 'a.md', 1)`)
	// A chunk that names its organisation, and one that leaves it to the v28 trigger.
	exec("knowledge chunks", `INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding, embedding_identity) VALUES
		('p48-kc', 'p48-kd', 'p48-org', 'x', '[]', NULL), ('p48-kc-blank', 'p48-kd', '', 'y', '[]', NULL)`)
	exec("flow", `INSERT INTO flows (id, name, owner_id) VALUES ('p48-flow', 'F', 'p48-u')`)
	exec("share link", `INSERT INTO share_tokens (id, flow_id, token_hash, created_by, created_at, expires_at) VALUES ('p48-share', 'p48-flow', 'p48-hash', 'p48-u', NOW(), NULL)`)
	exec("invite", `INSERT INTO org_invites (id, org_id, email, role, invited_by, token_hash, expires_at, accepted_at, created_at)
		VALUES ('p48-inv', 'p48-org', 'p48-i@example.com', 'member', 'p48-u', 'p48-inv-hash', NOW() + INTERVAL '1 day', NULL, NOW())`)
	exec("chat about an unsaved file", `INSERT INTO conversations (flow_id, scope, messages, updated_at) VALUES ('p48-no-flow', 'openai', '[]', NOW())`)

	var blankOrg string
	if err := b.db.QueryRowContext(ctx, `SELECT org_id FROM knowledge_chunks WHERE id = 'p48-kc-blank'`).Scan(&blankOrg); err != nil || blankOrg != "p48-org" {
		t.Errorf("the chunk without an organisation got %q, %v; want its document's (the v28 trigger runs before the foreign key)", blankOrg, err)
	}

	// The organisation-delete and erasure statements of that release, which the cascades now back.
	exec("organisation delete", `DELETE FROM organisations WHERE id = 'p48-org'`)
	exec("account erasure", `DELETE FROM users WHERE id = 'p48-u'`)
	var left int
	if err := b.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM policies WHERE org_id = 'p48-org') + (SELECT count(*) FROM org_channels WHERE org_id = 'p48-org')
		+ (SELECT count(*) FROM org_custom_rules WHERE org_id = 'p48-org') + (SELECT count(*) FROM knowledge_documents WHERE org_id = 'p48-org')
		+ (SELECT count(*) FROM knowledge_chunks WHERE doc_id = 'p48-kd') + (SELECT count(*) FROM refresh_tokens WHERE user_id = 'p48-u')`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows of the deleted organisation and account are left, %v; want the cascades to take them", left, err)
	}
}

// seedOrgRow stores organisation id, with no members, for a test whose organisation-scoped rows (knowledge base,
// policies, channels, custom rules) need it since v48, and deletes it, with those rows, when the test ends.
func seedOrgRow(t testing.TB, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO organisations (id, name, owner_id) VALUES ($1, $1, '') ON CONFLICT (id) DO NOTHING`, id); err != nil {
		t.Fatalf("seed organisation %s: %v", id, err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), `DELETE FROM organisations WHERE id = $1`, id) })
}

// TestPostgres_SaveOrgInvite_ConcurrentReplacementsOfAnExpiredInvite: several admins re-invite an address whose only
// pending invite has expired, at once. The delete and the insert share a transaction, so exactly one new invite is
// stored, the others answer ErrOrgInviteExists (they wait on the expired row, then meet the winner's), and the expired
// one is gone.
func TestPostgres_SaveOrgInvite_ConcurrentReplacementsOfAnExpiredInvite(t *testing.T) {
	f := newR2Fixture(t)
	org := f.org("inv-race", "owner", nil)
	email := f.id("inv-race") + "@example.com"
	invite := func(name string, expires time.Time) *interfaces.OrgInvite {
		return &interfaces.OrgInvite{ID: f.id("inv-" + name), OrgID: org, Email: email, Role: auth.RoleMember,
			InvitedBy: "owner", TokenHash: f.id("hash-" + name), ExpiresAt: expires}
	}
	if err := f.b.SaveOrgInvite(f.ctx, invite("stale", time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.b.SaveOrgInvite(f.ctx, invite(fmt.Sprintf("new-%d", i), time.Now().Add(time.Hour)))
		}()
	}
	wg.Wait()
	close(errs)
	stored := 0
	for err := range errs {
		switch {
		case err == nil:
			stored++
		case !errors.Is(err, interfaces.ErrOrgInviteExists):
			t.Errorf("a concurrent re-invite: %v, want nil or ErrOrgInviteExists", err)
		}
	}
	list, err := f.b.ListOrgInvites(f.ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 1 || len(list) != 1 || list[0].ID == f.id("inv-stale") {
		t.Errorf("%d saves succeeded, the org lists %d invites (%+v); want one new invite and the expired one gone", stored, len(list), list)
	}
}
