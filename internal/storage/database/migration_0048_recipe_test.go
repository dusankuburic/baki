package database

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// The remedy operations.md gives for accounts migration 0048 refuses (emails equal up to case), as an operator runs
// it in psql as the app role; the placeholders are the only edits. List the groups, compare each group's accounts,
// keep the one its owner uses (a verified one before an unverified one: anyone could register an address; then
// sign-ins, SSO links, API tokens, flows), then resolve the group in one transaction.
const (
	duplicateEmailGroupsSQL  = DuplicateEmailGroupsSQL
	duplicateEmailCompareSQL = DuplicateEmailCompareSQL
	duplicateEmailResolveSQL = DuplicateEmailResolveSQL
)

// resolveDuplicateEmail runs the recipe's transaction for one group: kept stays, others are renamed.
func resolveDuplicateEmail(t *testing.T, db *sql.DB, kept string, others ...string) {
	t.Helper()
	quoted := make([]string, len(others))
	for i, id := range others {
		quoted[i] = "'" + id + "'"
	}
	stmt := strings.NewReplacer("<kept>", kept, "<others>", strings.Join(quoted, ", ")).Replace(duplicateEmailResolveSQL)
	if _, err := db.ExecContext(context.Background(), stmt); err != nil {
		t.Fatalf("the recipe's transaction: %v\n%s", err, stmt)
	}
}

// TestMigration0048_DuplicateEmailRecipe follows the recipe on what the release before let happen: a mixed-case
// account (the older, verified one its owner signs in with) and its lower-case twin, which holds the owner's SSO
// link, a session, an API token and a password-reset link, and an unverified shadow holding an SSO link; and a
// group of two unverified accounts. Afterwards the owner signs in by password, reset and SSO as before, now into the
// kept account; the renamed accounts have no way in; and the step applies.
func TestMigration0048_DuplicateEmailRecipe(t *testing.T) {
	defer auth.SetBcryptCostForTesting(4)()
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if rolled, err := b.MigrateDown(ctx, 47); err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 48 {
		t.Fatalf("MigrateDown(47) = %v, %v", rolled, err)
	}
	hash := func(pw string) string {
		h, err := auth.HashPassword(pw)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := b.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	day := time.Now().Add(24 * time.Hour)
	exec(`INSERT INTO users (id, email, password, role, email_verified, created_at) VALUES
		('rcp-kept', 'Rcp-Owner@Example.com', $1, 'member', TRUE, NOW() - INTERVAL '2 days'),
		('rcp-twin', 'rcp-owner@example.com', $2, 'member', TRUE, NOW() - INTERVAL '1 day'),
		('rcp-shadow', 'RCP-OWNER@example.com', $3, 'member', FALSE, NOW() - INTERVAL '12 hours'),
		('rcp-u1', 'Rcp-Unverified@example.com', $3, 'member', FALSE, NOW() - INTERVAL '2 days'),
		('rcp-u2', 'rcp-unverified@example.com', $3, 'member', FALSE, NOW() - INTERVAL '1 day')`,
		hash("kept-pw"), hash("twin-pw"), hash("u-pw"))
	exec(`INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ('rcp-rt-kept', 'rcp-kept', $1), ('rcp-rt-twin', 'rcp-twin', $1)`, day)
	exec(`INSERT INTO api_tokens (id, user_id, name, token_hash) VALUES ('rcp-at-kept', 'rcp-kept', 'ci', 'rcp-at-kept-hash'), ('rcp-at-twin', 'rcp-twin', 'ci', 'rcp-at-twin-hash')`)
	exec(`INSERT INTO user_tokens (token_hash, purpose, user_id, expires_at) VALUES ('rcp-reset-twin', $1, 'rcp-twin', $2)`, interfaces.TokenPurposePasswordReset, day)
	exec(`INSERT INTO identity_links (provider, subject, user_id, email) VALUES ('oidc', 'rcp-sub', 'rcp-twin', 'rcp-owner@example.com'),
		('oidc', 'rcp-sub-shadow', 'rcp-shadow', 'rcp-owner@example.com'),
		('oidc', 'rcp-sub-u2', 'rcp-u2', 'rcp-unverified@example.com')`)

	// The operator's view: both groups, then each group's accounts, verified first.
	groups := func() []string {
		rows, err := b.db.QueryContext(ctx, duplicateEmailGroupsSQL)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var groups []string
		for rows.Next() {
			var address, ids string
			if err := rows.Scan(&address, &ids); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(address, "rcp-") {
				groups = append(groups, address+": "+ids)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return groups
	}()
	if want := "rcp-owner@example.com: rcp-kept, rcp-twin, rcp-shadow|rcp-unverified@example.com: rcp-u1, rcp-u2"; strings.Join(groups, "|") != want {
		t.Fatalf("the group query = %q, want %q", strings.Join(groups, "|"), want)
	}
	var first string
	var verified, hasPassword bool
	var links, tokens int
	compare := strings.ReplaceAll(duplicateEmailCompareSQL, "<address>", "rcp-owner@example.com")
	if err := b.db.QueryRowContext(ctx, `SELECT id, email_verified, has_password, sso_links, api_tokens FROM (`+compare+`) c WHERE id = 'rcp-twin'`).
		Scan(&first, &verified, &hasPassword, &links, &tokens); err != nil || !verified || !hasPassword || links != 1 || tokens != 1 {
		t.Fatalf("the comparison of rcp-twin = verified %v, password %v, %d SSO links, %d API tokens, %v; want true, true, 1, 1", verified, hasPassword, links, tokens, err)
	}

	resolveDuplicateEmail(t, b.db, "rcp-kept", "rcp-twin", "rcp-shadow")
	resolveDuplicateEmail(t, b.db, "rcp-u1", "rcp-u2")

	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("the step after the recipe: %v", err)
	}
	t.Cleanup(func() { b2.Close() })
	if v, _ := b2.CurrentSchemaVersion(ctx); v < 48 {
		t.Fatalf("schema at v%d, want 48", v)
	}

	// The owner: sign-in by the address in any case, the kept password, the kept session and API token, and the SSO
	// identity the twin held.
	for _, typed := range []string{"rcp-owner@example.com", "Rcp-Owner@Example.com"} {
		u, err := b2.LoadUserByEmail(ctx, typed)
		if err != nil || u.ID != "rcp-kept" || !auth.CheckPasswordHash("kept-pw", u.Password) {
			t.Errorf("sign-in as %q finds %+v, %v; want rcp-kept with its password", typed, u, err)
		}
	}
	if u, err := b2.LoadUserByEmail(ctx, "rcp-unverified@example.com"); err != nil || u.ID != "rcp-u1" {
		t.Errorf("sign-in as the unverified address finds %+v, %v; want rcp-u1", u, err)
	}
	if link, err := b2.LoadIdentityLink(ctx, "oidc", "rcp-sub"); err != nil || link.UserID != "rcp-kept" {
		t.Errorf("the owner's SSO identity = %+v, %v; want it linked to rcp-kept", link, err)
	}
	if _, err := b2.GetAPITokenByHash(ctx, "rcp-at-kept-hash"); err != nil {
		t.Errorf("the kept account's API token: %v", err)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := b2.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM refresh_tokens WHERE jti = 'rcp-rt-kept' AND NOT revoked`); n != 1 {
		t.Errorf("the kept account's session was revoked")
	}

	// The renamed accounts: no address anyone types, no password, session, API token, reset link or SSO identity.
	for _, id := range []string{"rcp-twin", "rcp-shadow", "rcp-u2"} {
		u, err := b2.LoadUserByID(ctx, id)
		if err != nil || u.Email != "duplicate+"+id+"@example.invalid" || u.Password != "" {
			t.Errorf("%s = %+v, %v; want the duplicate address and no password", id, u, err)
		}
	}
	if u, err := b2.LoadUserByID(ctx, "rcp-twin"); err == nil && auth.CheckPasswordHash("twin-pw", u.Password) {
		t.Error("the twin's password still signs in")
	}
	if n := count(`SELECT count(*) FROM refresh_tokens WHERE user_id IN ('rcp-twin', 'rcp-shadow', 'rcp-u2') AND NOT revoked`); n != 0 {
		t.Errorf("%d sessions of the renamed accounts are live", n)
	}
	if _, err := b2.GetAPITokenByHash(ctx, "rcp-at-twin-hash"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("the twin's API token: %v; want ErrNotFound", err)
	}
	if uid, err := b2.ConsumeUserToken(ctx, interfaces.TokenPurposePasswordReset, "rcp-reset-twin"); err == nil {
		t.Errorf("the twin's password-reset link still works (for %s)", uid)
	}
	if n := count(`SELECT count(*) FROM identity_links WHERE user_id IN ('rcp-twin', 'rcp-shadow', 'rcp-u2')`); n != 0 {
		t.Errorf("%d SSO identities still sign in to a renamed account", n)
	}
	// An unverified account neither takes nor gives an SSO identity: its holder never proved the address, so its
	// link must not reach the verified owner (sign-in refuses to link a verified account by email, too). An
	// unverified kept account gets SSO by the next sign-in's linking, with its hardening (password cleared).
	if _, err := b2.LoadIdentityLink(ctx, "oidc", "rcp-sub-shadow"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("the unverified shadow's SSO identity: %v; want it unlinked, not moved to the verified owner", err)
	}
	if _, err := b2.LoadIdentityLink(ctx, "oidc", "rcp-sub-u2"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("the unverified group's SSO identity: %v; want it unlinked", err)
	}
}

// TestMigration0048_DeploymentDocRecipeMatchesConstants asserts that the fenced SQL blocks in
// docs/deployment.md match the exported preflight and resolution SQL constants in migration_0048_recipe.go.
func TestMigration0048_DeploymentDocRecipeMatchesConstants(t *testing.T) {
	docPath := "../../../docs/deployment.md"
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	for name, sqlConst := range map[string]string{
		"DuplicateEmailGroupsSQL":       DuplicateEmailGroupsSQL,
		"DuplicateInviteTokenGroupsSQL": DuplicateInviteTokenGroupsSQL,
		"DuplicateEmailCompareSQL":      DuplicateEmailCompareSQL,
		"DuplicateEmailResolveSQL":      DuplicateEmailResolveSQL,
	} {
		expected := strings.ReplaceAll(strings.TrimSpace(sqlConst), "\r\n", "\n")
		if !strings.Contains(content, expected) {
			t.Errorf("docs/deployment.md does not contain %s verbatim:\n%s", name, expected)
		}
	}
}
