package database

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pad-analyzer/internal/storage/interfaces"
)

// azureRegistryV25Checksum pins v25's SQL. Once v25 has shipped, a change
// to its text fails every deployed database's boot (verifyChecksums): fix
// forward with a v26 instead of editing v25.
const azureRegistryV25Checksum = "433e9507d591dcecda3ef7f2008eaede5c9201c32c5b1dbab40814d58a44ddf2"

var (
	azureUserTables = []string{"user_azure_connections", "user_azure_models", "user_azure_registry_state"}
	azureOrgTables  = []string{"org_azure_connections", "org_azure_models", "org_azure_registry_state"}
)

func azureTables() []string { return append(append([]string{}, azureUserTables...), azureOrgTables...) }

// createTableBlock returns the CREATE TABLE statement for table in sql.
func createTableBlock(t *testing.T, sql, table string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS ` + table + ` \((.*?)\n\);`)
	m := re.FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("v25 has no CREATE TABLE for %s", table)
	}
	return m[1]
}

// TestMigration25_AzureRegistryShape pins the v25 step without a database:
// the six tables, FORCE-d RLS with a policy each, no user-id column in any
// org row, and a down step that removes only the family members'
// credentials.
func TestMigration25_AzureRegistryShape(t *testing.T) {
	m, ok := migrationByVersion(25)
	if !ok {
		t.Fatal("migration v25 not registered")
	}
	if m.name != "azure_registry" {
		t.Errorf("v25 name = %q, want azure_registry", m.name)
	}
	for _, table := range azureTables() {
		block := createTableBlock(t, m.sql, table)
		for _, want := range []string{
			"ALTER TABLE " + table + " ",
			"CREATE POLICY rls_" + table + "_visible ON " + table + " FOR ALL USING",
		} {
			if !strings.Contains(m.sql, want) {
				t.Errorf("v25 lacks %q", want)
			}
		}
		if !regexp.MustCompile(`ALTER TABLE ` + table + `\s+FORCE\s+ROW LEVEL SECURITY`).MatchString(m.sql) {
			t.Errorf("v25 must FORCE row-level security on %s (ENABLE alone exempts the owning app role)", table)
		}
		if !regexp.MustCompile(`ALTER TABLE ` + table + `\s+ENABLE\s+ROW LEVEL SECURITY`).MatchString(m.sql) {
			t.Errorf("v25 must ENABLE row-level security on %s", table)
		}
		if strings.HasPrefix(table, "org_") && regexp.MustCompile(`\b(user_id|\w+_by)\b`).MatchString(block) {
			t.Errorf("%s must hold no user ids (erasure never rewrites org rows); got:\n%s", table, block)
		}
		if strings.Contains(block, "sort_order") || strings.Contains(block, "status") {
			t.Errorf("%s must store neither sort_order nor status; got:\n%s", table, block)
		}
		if !strings.Contains(block, "ON DELETE CASCADE") {
			t.Errorf("%s must cascade with its owner", table)
		}
	}
	for _, want := range []string{
		"(user_id, connection_id, (CASE kind WHEN 'claude' THEN 'chat' ELSE kind END), lower(deployment))",
		"(org_id, connection_id, (CASE kind WHEN 'claude' THEN 'chat' ELSE kind END), lower(deployment))",
		"user_azure_models_kb_idx ON user_azure_models (user_id) WHERE is_kb_embedding",
		"org_azure_models_kb_idx ON org_azure_models (org_id) WHERE is_kb_embedding",
		"org_azure_models_default_idx ON org_azure_models (org_id) WHERE is_default",
		"user_azure_connections_name_idx ON user_azure_connections (user_id, lower(name))",
		"org_azure_connections_name_idx ON org_azure_connections (org_id, lower(name))",
		"NOT app_rls_active() OR user_id = app_current_user_id()",
		"EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = org_azure_models.org_id AND om.user_id = app_current_user_id())",
	} {
		if !strings.Contains(m.sql, want) {
			t.Errorf("v25 lacks %q", want)
		}
	}
	if strings.Contains(createTableBlock(t, m.sql, "user_azure_models"), "is_default") {
		t.Error("user models must not carry is_default (users have no default for members)")
	}

	for _, table := range azureTables() {
		if !strings.Contains(m.downSQL, "DROP TABLE IF EXISTS "+table+";") {
			t.Errorf("v25 down must drop %s", table)
		}
	}
	for _, want := range []string{
		"DELETE FROM provider_keys WHERE starts_with(provider, 'azure-foundry/')",
		"DELETE FROM org_provider_keys WHERE starts_with(provider, 'azure-foundry/')",
	} {
		if !strings.Contains(m.downSQL, want) {
			t.Errorf("v25 down lacks %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(m.downSQL), " LIKE ") || strings.Contains(m.downSQL, "provider = 'azure-foundry'") {
		t.Errorf("v25 down must match the family members with starts_with only, never LIKE or the exact default key:\n%s", m.downSQL)
	}
}

// TestMigration25_ChecksumPinned fails when v25's SQL text changes.
func TestMigration25_ChecksumPinned(t *testing.T) {
	m, _ := migrationByVersion(25)
	if got := migrationChecksum(m.sql); got != azureRegistryV25Checksum {
		t.Errorf("v25 checksum = %s, want the pinned %s — a shipped migration must never change; add v26 instead", got, azureRegistryV25Checksum)
	}
}

// TestMigration25_ChecksumRecorded: a migrated database records v25 under
// the pinned checksum.
func TestMigration25_ChecksumRecorded(t *testing.T) {
	b := openMigrationTestDB(t)
	var got string
	if err := b.db.QueryRowContext(context.Background(), `SELECT checksum FROM schema_migrations WHERE version = 25`).Scan(&got); err != nil {
		t.Fatalf("read the v25 checksum: %v", err)
	}
	if got != azureRegistryV25Checksum {
		t.Errorf("recorded v25 checksum = %s, want %s", got, azureRegistryV25Checksum)
	}
}

// TestMigration25_RLSForcedAndPolicies checks the catalog: all six tables
// have row-level security enabled AND forced, and exactly one ALL policy
// whose USING and WITH CHECK both short-circuit on app_rls_active() and
// then test the owner — the caller's own id for user tables, membership for
// org tables. Catalog queries run whatever role the tests connect as.
func TestMigration25_RLSForcedAndPolicies(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	for _, table := range azureTables() {
		var enabled, forced bool
		if err := b.db.QueryRowContext(ctx, `
			SELECT c.relrowsecurity, c.relforcerowsecurity
			  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = current_schema() AND c.relname = $1`, table).Scan(&enabled, &forced); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if !enabled || !forced {
			t.Errorf("%s: rls enabled=%v forced=%v, want both", table, enabled, forced)
		}
		policies := tablePolicies(t, b, table)
		if len(policies) != 1 {
			t.Fatalf("%s has policies %+v, want exactly one", table, policies)
		}
		p := policies[0]
		if p.name != "rls_"+table+"_visible" || p.cmd != "ALL" {
			t.Errorf("%s: policy %s for %s, want rls_%s_visible for ALL", table, p.name, p.cmd, table)
		}
		for what, expr := range map[string]string{"USING": p.qual, "WITH CHECK": p.check} {
			if !strings.Contains(expr, "app_rls_active()") || !strings.Contains(expr, "app_current_user_id()") {
				t.Errorf("%s %s = %q, want the app_rls_active() guard and the caller check", table, what, expr)
			}
			if strings.HasPrefix(table, "user_") && !strings.Contains(expr, "user_id") {
				t.Errorf("%s %s = %q, want the caller's own rows", table, what, expr)
			}
			if strings.HasPrefix(table, "org_") && !strings.Contains(expr, "org_members") {
				t.Errorf("%s %s = %q, want the org membership predicate", table, what, expr)
			}
		}
	}
}

type catalogPolicy struct{ name, cmd, qual, check string }

// tablePolicies reads table's row-level-security policies from pg_policies.
func tablePolicies(t *testing.T, b *PostgresStorageBackend, table string) []catalogPolicy {
	t.Helper()
	rows, err := b.db.QueryContext(context.Background(), `
		SELECT policyname, cmd, coalesce(qual, ''), coalesce(with_check, '')
		  FROM pg_policies WHERE schemaname = current_schema() AND tablename = $1`, table)
	if err != nil {
		t.Fatalf("%s policies: %v", table, err)
	}
	defer rows.Close()
	var out []catalogPolicy
	for rows.Next() {
		var p catalogPolicy
		if err := rows.Scan(&p.name, &p.cmd, &p.qual, &p.check); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// v25TestOwners creates a user and an org the constraint tests can own rows
// with, removed on cleanup (their rows cascade).
func v25TestOwners(t *testing.T, b *PostgresStorageBackend) (userID, orgID string) {
	t.Helper()
	ctx := context.Background()
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, orgID = "v25-user-"+stamp, "v25-org-"+stamp
	if err := b.CreateUser(ctx, &interfaces.User{ID: userID, Email: userID + "@example.com", Password: "x", Role: "member",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "v25", OwnerID: userID}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	t.Cleanup(func() {
		_ = b.DeleteOrg(context.Background(), orgID)
		_, _ = b.db.ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	return userID, orgID
}

// TestMigration25_CheckConstraints writes raw rows — no Go validation in
// the way — so the SQL CHECKs themselves are shown to hold the rules
// interfaces.ValidateAzure*Record mirrors, each under its own constraint
// name (the store maps those names), and the unique indexes to hold the
// namespace and designation rules.
func TestMigration25_CheckConstraints(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	uid, oid := v25TestOwners(t, b)

	exec := func(q string, args ...any) error {
		// Each probe in its own transaction, rolled back: nothing persists.
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		_, err = tx.ExecContext(ctx, q, args...)
		return err
	}
	wantViolation := func(what, code, constraint string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code || pgErr.ConstraintName != constraint {
			t.Errorf("%s: err = %v, want SQLSTATE %s on %s", what, err, code, constraint)
		}
	}
	const (
		conn  = `INSERT INTO user_azure_connections (user_id, id, name) VALUES ($1, $2, $3)`
		model = `WITH c AS (INSERT INTO user_azure_connections (user_id, id, name) VALUES ($1, 'default', 'res'))
			INSERT INTO user_azure_models (user_id, id, connection_id, kind, deployment, display_name, purpose,
				purpose_custom, description, context_window, max_output_tokens, is_kb_embedding)
			VALUES ($1, 'abcdefghij', 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10)`
		orgModel = `WITH c AS (INSERT INTO org_azure_connections (org_id, id, name) VALUES ($1, 'default', 'res'))
			INSERT INTO org_azure_models (org_id, id, connection_id, kind, deployment, display_name, is_default)
			VALUES ($1, 'abcdefghij', 'default', $2, 'dep', 'Dep', $3)`
	)
	type modelRow struct {
		kind, deployment, display, purpose, custom, description string
		window, output                                          any
		kb                                                      bool
	}
	good := modelRow{"chat", "gpt-4.1", "GPT", "general", "", "", nil, nil, false}
	insertModel := func(r modelRow) error {
		return exec(model, uid, r.kind, r.deployment, r.display, r.purpose, r.custom, r.description, r.window, r.output, r.kb)
	}

	const check = pgErrCheckViolation
	wantViolation("an uppercase resource id", check, "user_azure_connections_id_check", exec(conn, uid, "Default", "a"))
	wantViolation("an empty resource name", check, "user_azure_connections_name_check", exec(conn, uid, "abcdefghij", ""))
	wantViolation("a 65-character resource name", check, "user_azure_connections_name_check", exec(conn, uid, "abcdefghij", strings.Repeat("n", 65)))
	if err := exec(conn, uid, "abcdefghij", strings.Repeat("é", 64)); err != nil {
		t.Errorf("64 two-byte characters (char_length counts characters): %v", err)
	}
	wantViolation("a test result without a time", check, "user_azure_connections_last_test_check",
		exec(`INSERT INTO user_azure_connections (user_id, id, name, last_test_ok) VALUES ($1, 'abcdefghij', 'x', TRUE)`, uid))
	wantViolation("a 501-character test error", check, "user_azure_connections_last_test_check",
		exec(`INSERT INTO user_azure_connections (user_id, id, name, last_test_ok, last_test_at, last_test_error) VALUES ($1, 'abcdefghij', 'x', FALSE, NOW(), $2)`, uid, strings.Repeat("e", 501)))

	if err := insertModel(good); err != nil {
		t.Fatalf("a valid model: %v", err)
	}
	for _, tc := range []struct {
		what, constraint string
		mut              func(r *modelRow)
	}{
		{"an unknown kind", "user_azure_models_kind_check", func(r *modelRow) { r.kind = "gpt" }},
		{"a deployment with a slash", "user_azure_models_deployment_check", func(r *modelRow) { r.deployment = "a/b" }},
		{"a 65-character deployment", "user_azure_models_deployment_check", func(r *modelRow) { r.deployment = strings.Repeat("d", 65) }},
		{"an agent name with a tab", "user_azure_models_deployment_check", func(r *modelRow) { r.kind, r.deployment = "agent", "bad\tname" }},
		{"an agent name with DEL", "user_azure_models_deployment_check", func(r *modelRow) { r.kind, r.deployment = "agent", "bad\x7fname" }},
		{"a 129-character agent name", "user_azure_models_deployment_check", func(r *modelRow) { r.kind, r.deployment = "agent", strings.Repeat("a", 129) }},
		{"an empty display name", "user_azure_models_display_name_check", func(r *modelRow) { r.display = "" }},
		{"custom without text", "user_azure_models_purpose_check", func(r *modelRow) { r.purpose = "custom" }},
		{"a preset with text", "user_azure_models_purpose_check", func(r *modelRow) { r.custom = "x" }},
		{"33 characters of custom purpose", "user_azure_models_purpose_check", func(r *modelRow) { r.purpose, r.custom = "custom", strings.Repeat("p", 33) }},
		{"a 201-character description", "user_azure_models_description_check", func(r *modelRow) { r.description = strings.Repeat("d", 201) }},
		{"a context window of 1023", "user_azure_models_limits_check", func(r *modelRow) { r.window = 1023 }},
		{"max output of 1,000,001", "user_azure_models_limits_check", func(r *modelRow) { r.output = 1_000_001 }},
		{"an embedding with a context window", "user_azure_models_limits_check", func(r *modelRow) { r.kind, r.window = "embedding", 8192 }},
		{"a chat model as the knowledge-base model", "user_azure_models_kb_check", func(r *modelRow) { r.kb = true }},
	} {
		r := good
		tc.mut(&r)
		wantViolation(tc.what, check, tc.constraint, insertModel(r))
	}
	for _, tc := range []struct {
		what string
		mut  func(r *modelRow)
	}{
		{"a 128-character agent name with spaces", func(r *modelRow) { r.kind, r.deployment = "agent", "Review bot "+strings.Repeat("r", 117) }},
		{"32 two-byte characters of custom purpose", func(r *modelRow) { r.purpose, r.custom = "custom", strings.Repeat("ü", 32) }},
		{"the widest limits", func(r *modelRow) { r.window, r.output = 2_000_000, 1_000_000 }},
		{"a knowledge-base embedding", func(r *modelRow) { r.kind, r.kb = "embedding", true }},
	} {
		r := good
		tc.mut(&r)
		if err := insertModel(r); err != nil {
			t.Errorf("%s: %v, want accepted", tc.what, err)
		}
	}
	wantViolation("an embedding as the org default", check, "org_azure_models_default_check", exec(orgModel, oid, "embedding", true))
	if err := exec(orgModel, oid, "agent", true); err != nil {
		t.Errorf("an agent as the org default: %v", err)
	}

	// The unique indexes: claude shares chat's namespace, case-insensitively.
	wantViolation("claude GPT-4.1 next to chat gpt-4.1", pgErrUniqueViolation, "user_azure_models_deployment_idx", exec(`
		WITH c AS (INSERT INTO user_azure_connections (user_id, id, name) VALUES ($1, 'default', 'res')),
		     m AS (INSERT INTO user_azure_models (user_id, id, connection_id, kind, deployment, display_name)
		           VALUES ($1, 'aaaaaaaaaa', 'default', 'chat', 'gpt-4.1', 'a'))
		INSERT INTO user_azure_models (user_id, id, connection_id, kind, deployment, display_name)
		VALUES ($1, 'bbbbbbbbbb', 'default', 'claude', 'GPT-4.1', 'b')`, uid))
	wantViolation("a second resource named in another case", pgErrUniqueViolation, "user_azure_connections_name_idx", exec(`
		INSERT INTO user_azure_connections (user_id, id, name) VALUES ($1, 'aaaaaaaaaa', 'Contoso'), ($1, 'bbbbbbbbbb', 'CONTOSO')`, uid))
	wantViolation("two org defaults", pgErrUniqueViolation, "org_azure_models_default_idx", exec(`
		WITH c AS (INSERT INTO org_azure_connections (org_id, id, name) VALUES ($1, 'default', 'res'))
		INSERT INTO org_azure_models (org_id, id, connection_id, kind, deployment, display_name, is_default)
		VALUES ($1, 'aaaaaaaaaa', 'default', 'chat', 'a', 'a', TRUE), ($1, 'bbbbbbbbbb', 'default', 'chat', 'b', 'b', TRUE)`, oid))
}

// TestMigration25_DownDeletesOnlyFamilyMembers runs v25's down step inside
// a transaction that is rolled back — the shared database is never left
// without v25 — and checks it drops the six tables and deletes only the
// "azure-foundry/<id>" credentials: never the default resource's exact
// "azure-foundry" key, never another provider that merely starts with the
// same letters.
func TestMigration25_DownDeletesOnlyFamilyMembers(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	uid, oid := v25TestOwners(t, b)
	providers := []string{"azure-foundry", "azure-foundryX", "azure-foundry-legacy", "azure-foundry/abcdefghij", "azure-foundry/klmnopqrst"}
	for _, p := range providers {
		if _, err := b.db.ExecContext(ctx, `INSERT INTO provider_keys (user_id, provider, ciphertext) VALUES ($1, $2, 'ct')`, uid, p); err != nil {
			t.Fatalf("seed personal %s: %v", p, err)
		}
		if _, err := b.db.ExecContext(ctx, `INSERT INTO org_provider_keys (org_id, provider, ciphertext) VALUES ($1, $2, 'ct')`, oid, p); err != nil {
			t.Fatalf("seed org %s: %v", p, err)
		}
	}
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(), `DELETE FROM provider_keys WHERE user_id = $1`, uid)
	})

	m, _ := migrationByVersion(25)
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.downSQL); err != nil {
		t.Fatalf("v25 down: %v", err)
	}
	for _, table := range azureTables() {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("v25 down left %s", table)
		}
	}
	left := func(q, owner string) []string {
		rows, err := tx.QueryContext(ctx, q, owner)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want := "azure-foundry,azure-foundry-legacy,azure-foundryX"
	if got := strings.Join(left(`SELECT provider FROM provider_keys WHERE user_id = $1 ORDER BY provider COLLATE "C"`, uid), ","); got != want {
		t.Errorf("personal keys after v25 down = %s, want %s", got, want)
	}
	if got := strings.Join(left(`SELECT provider FROM org_provider_keys WHERE org_id = $1 ORDER BY provider COLLATE "C"`, oid), ","); got != want {
		t.Errorf("org keys after v25 down = %s, want %s", got, want)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// The rollback restored everything.
	var tables, keys int
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM unnest($1::text[]) t WHERE to_regclass(t) IS NOT NULL`, azureTables()).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM provider_keys WHERE user_id = $1`, uid).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if tables != 6 || keys != len(providers) {
		t.Errorf("after the rollback: %d tables, %d keys; want 6 and %d", tables, keys, len(providers))
	}
}
