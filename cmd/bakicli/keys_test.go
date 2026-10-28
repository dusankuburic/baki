package main

import (
	"context"
	"strings"
	"testing"
)

const (
	keysTestAuthSecret = "keys-test-auth-secret-0123456789abcdefghij"
	keysTestNewKey     = "keys-test-new-encryption-key-0123456789abc"
)

func TestBakicliKeys_Usage(t *testing.T) {
	_, stderr, code := runBakicli(t, "keys")
	if code != 2 || !strings.Contains(stderr, "usage: bakicli keys check") {
		t.Errorf("bakicli keys: exit %d, stderr %q; want 2 and the usage", code, stderr)
	}
	_, stderr, code = runBakicliIn(t, ".", []string{"PAD_DATABASE_URL="}, "keys", "check")
	if code != 2 || !strings.Contains(stderr, "no database DSN") {
		t.Errorf("bakicli keys check without a DSN: exit %d, stderr %q; want 2", code, stderr)
	}
}

// TestBakicliKeysCheck walks a deployment off the PAD_AUTH_SECRET fallback (R-security-7): on the fallback the check
// says a JWT rotation would lose the stored values and what to set; with a new key and the auth secret listed as the
// previous one it reports the re-seal pending; after the re-seal it is clean; and with the new key but the auth secret
// not listed it reports the values the server can no longer open. No output ever carries a key.
func TestBakicliKeysCheck(t *testing.T) {
	b, dsn := openIsolatedMigrateCLIDB(t)
	ctx := context.Background()
	if _, err := b.DB().ExecContext(ctx, `INSERT INTO organisations (id, name, owner_id) VALUES ('keys-org', 'Keys', 'keys-user')`); err != nil {
		t.Fatal(err)
	}
	fallback, err := b.NewEncryptedKeyStore([]byte(keysTestAuthSecret))
	if err != nil {
		t.Fatal(err)
	}
	if err := fallback.Save("keys-user", "openai", "sk-user"); err != nil {
		t.Fatal(err)
	}
	if err := fallback.SaveOrg("keys-org", "azure-foundry", `{"apiKey":"org"}`, "keys-user"); err != nil {
		t.Fatal(err)
	}

	check := func(env ...string) (string, int) {
		t.Helper()
		base := []string{"PAD_ENCRYPTION_KEY=", "PAD_ENCRYPTION_KEY_PREVIOUS=", "PAD_AUTH_SECRET=", "PAD_MODE=cloud"}
		stdout, stderr, code := runBakicliIn(t, ".", append(base, env...), "keys", "check", "--dsn", dsn)
		out := stdout + stderr
		for _, k := range []string{keysTestAuthSecret, keysTestNewKey} {
			if strings.Contains(out, k) {
				t.Fatalf("the check printed key material:\n%s", out)
			}
		}
		return out, code
	}
	wantIn := func(name, out string, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(out, w) {
				t.Errorf("%s: output lacks %q:\n%s", name, w, out)
			}
		}
	}

	out, code := check("PAD_AUTH_SECRET=" + keysTestAuthSecret)
	if code != 1 {
		t.Errorf("on the fallback: exit %d, want 1:\n%s", code, out)
	}
	wantIn("on the fallback", out, "PAD_AUTH_SECRET", "2 checked", "ACTION NEEDED", "PAD_ENCRYPTION_KEY_PREVIOUS")

	out, code = check("PAD_AUTH_SECRET="+keysTestAuthSecret, "PAD_ENCRYPTION_KEY="+keysTestNewKey, "PAD_ENCRYPTION_KEY_PREVIOUS="+keysTestAuthSecret)
	if code != 0 {
		t.Errorf("rotation under way: exit %d, want 0:\n%s", code, out)
	}
	wantIn("rotation under way", out, "PAD_ENCRYPTION_KEY_PREVIOUS entry 1 (same value as PAD_AUTH_SECRET): 2", "re-seal pending")

	out, code = check("PAD_AUTH_SECRET="+keysTestAuthSecret, "PAD_ENCRYPTION_KEY="+keysTestNewKey)
	if code != 1 {
		t.Errorf("new key without the previous one: exit %d, want 1:\n%s", code, out)
	}
	wantIn("new key without the previous one", out, "ACTION NEEDED", "only with PAD_AUTH_SECRET")

	rotated, err := b.NewEncryptedKeyStore([]byte(keysTestNewKey), []byte(keysTestAuthSecret))
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := rotated.ResealKeys(ctx); err != nil || rep.Resealed != 2 {
		t.Fatalf("re-seal: %+v, %v", rep, err)
	}
	out, code = check("PAD_AUTH_SECRET="+keysTestAuthSecret, "PAD_ENCRYPTION_KEY="+keysTestNewKey)
	if code != 0 {
		t.Errorf("after the re-seal: exit %d, want 0:\n%s", code, out)
	}
	wantIn("after the re-seal", out, "PAD_ENCRYPTION_KEY: 2", "ok:")
}
