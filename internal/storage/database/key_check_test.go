package database

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const (
	checkCurrentKey  = "check-current-key-0123456789abcdefghijkl"
	checkPreviousKey = "check-previous-key-0123456789abcdefghijk"
	checkAuthSecret  = "check-auth-secret-0123456789abcdefghijkl"
	checkStrayKey    = "check-stray-key-0123456789abcdefghijklmno"
)

// TestCheckSealKeys reports, for every sealed value, which configured key opens it (R-security-7's key check): the
// current key, a previous one, the auth secret beside them, or none. It reads and never writes, and its report names
// keys only by the names it was given.
func TestCheckSealKeys(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if _, err := b.db.ExecContext(ctx, `INSERT INTO organisations (id, name, owner_id) VALUES ('chk-org', 'Check', 'chk-user')`); err != nil {
		t.Fatal(err)
	}
	keystore := func(key string) *EncryptedKeyStore {
		t.Helper()
		ks, err := b.NewEncryptedKeyStore([]byte(key))
		if err != nil {
			t.Fatal(err)
		}
		return ks
	}
	// Two provider keys under the current key, one under the previous key, one under the auth secret (a deployment
	// that ran on the fallback), one under a key nobody configured; an organisation credential under the auth secret.
	for user, key := range map[string]string{"u1": checkCurrentKey, "u2": checkCurrentKey, "u3": checkPreviousKey, "u4": checkAuthSecret, "u5": checkStrayKey} {
		if err := keystore(key).Save("chk-"+user, "openai", "sk-"+user); err != nil {
			t.Fatal(err)
		}
	}
	if err := keystore(checkAuthSecret).SaveOrg("chk-org", "azure-foundry", `{"apiKey":"org"}`, "chk-user"); err != nil {
		t.Fatal(err)
	}
	// A channel url sealed under the previous key, and one stored before field encryption (plaintext).
	if err := b.EnableFieldEncryption([]byte(checkPreviousKey)); err != nil {
		t.Fatal(err)
	}
	sealed, err := b.sealField("org_channels", "url", "chk-ch1", "https://hooks.example.com/1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(ctx, `INSERT INTO org_channels (id, org_id, kind, url) VALUES ('chk-ch1', 'chk-org', 'webhook', $1), ('chk-ch2', 'chk-org', 'webhook', 'https://hooks.example.com/2')`, sealed); err != nil {
		t.Fatal(err)
	}
	before := snapshotSealed(t, b)

	keys := []SealKey{
		{Name: "PAD_ENCRYPTION_KEY", Key: []byte(checkCurrentKey)},
		{Name: "PAD_ENCRYPTION_KEY_PREVIOUS entry 1", Key: []byte(checkPreviousKey)},
		{Name: "PAD_AUTH_SECRET", Key: []byte(checkAuthSecret)},
	}
	rep, err := CheckSealKeys(ctx, b.DB(), keys)
	if err != nil {
		t.Fatalf("CheckSealKeys: %v", err)
	}
	want := map[string]int{"PAD_ENCRYPTION_KEY": 2, "PAD_ENCRYPTION_KEY_PREVIOUS entry 1": 2, "PAD_AUTH_SECRET": 2}
	for name, n := range want {
		if rep.OpenedBy[name] != n {
			t.Errorf("values %s opens = %d, want %d (report %+v)", name, rep.OpenedBy[name], n, rep)
		}
	}
	if rep.Checked != 7 || rep.Unreadable != 1 || rep.Plaintext != 1 {
		t.Errorf("checked %d, unreadable %d, plaintext %d; want 7, 1 and 1 (report %+v)", rep.Checked, rep.Unreadable, rep.Plaintext, rep)
	}
	if len(rep.UnreadableRefs) != 1 || rep.UnreadableRefs[0] != (SealedValueRef{"provider_keys", "ciphertext", "chk-u5/openai"}) {
		t.Errorf("unreadable refs = %+v, want provider_keys chk-u5/openai", rep.UnreadableRefs)
	}
	byColumn := map[string]SealKeyColumn{}
	for _, c := range rep.Columns {
		byColumn[c.Table+"."+c.Column] = c
	}
	if c := byColumn["org_provider_keys.ciphertext"]; c.Checked != 1 || c.OpenedBy["PAD_AUTH_SECRET"] != 1 {
		t.Errorf("org_provider_keys column = %+v, want its one value opened by the auth secret", c)
	}
	if c := byColumn["org_channels.url"]; c.Checked != 1 || c.Plaintext != 1 || c.OpenedBy["PAD_ENCRYPTION_KEY_PREVIOUS entry 1"] != 1 {
		t.Errorf("org_channels.url column = %+v, want one sealed under the previous key and one plaintext", c)
	}
	if s := fmt.Sprintf("%+v", rep); strings.Contains(s, checkCurrentKey) || strings.Contains(s, checkAuthSecret) || strings.Contains(s, checkPreviousKey) {
		t.Error("the report carries key material")
	}
	if after := snapshotSealed(t, b); after != before {
		t.Error("the check changed stored values; it must only read")
	}

	// A key listed twice (the auth secret's value reused as PAD_ENCRYPTION_KEY) counts under its first name.
	dup, err := CheckSealKeys(ctx, b.DB(), []SealKey{{Name: "PAD_ENCRYPTION_KEY", Key: []byte(checkAuthSecret)}, {Name: "PAD_AUTH_SECRET", Key: []byte(checkAuthSecret)}})
	if err != nil {
		t.Fatal(err)
	}
	if dup.OpenedBy["PAD_ENCRYPTION_KEY"] != 2 || dup.OpenedBy["PAD_AUTH_SECRET"] != 0 {
		t.Errorf("a repeated key: %+v, want its values under the first name", dup.OpenedBy)
	}
}

// snapshotSealed is every sealed column's values, to prove a check wrote nothing.
func snapshotSealed(t *testing.T, b *PostgresStorageBackend) string {
	t.Helper()
	var sb strings.Builder
	for _, q := range []string{
		`SELECT user_id || '/' || provider || '=' || ciphertext || '@' || updated_at::text FROM provider_keys ORDER BY 1`,
		`SELECT org_id || '/' || provider || '=' || ciphertext || '@' || updated_at::text FROM org_provider_keys ORDER BY 1`,
		`SELECT id || '=' || url FROM org_channels ORDER BY 1`,
	} {
		lines, err := queryLines(b, q)
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString(lines)
	}
	return sb.String()
}

// queryLines is a one-column query's rows, a line each.
func queryLines(b *PostgresStorageBackend, q string) (string, error) {
	rows, err := b.db.QueryContext(context.Background(), q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
		sb.WriteString(s)
		sb.WriteByte('\n')
	}
	return sb.String(), rows.Err()
}
