package database_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/interfaces"
)

// R-provider-19: the provider catalogue and listing read every key of a scope in one query — KeyStates for the
// caller, OrgKeyStates for their organisation — where they probed each spec's key on its own, and the reads run
// under the request's context, the store's 5 s timeout only their ceiling.

// TestKeystoreStates_OneQueryPerScope: a usable key, a legacy empty row and a row the deployment key no longer
// opens are told apart without their values leaving the store; a key not asked for, another owner's row and a key
// with no row have no entry.
func TestKeystoreStates_OneQueryPerScope(t *testing.T) {
	b, ks := openFamilyKeystore(t)
	ctx := context.Background()
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	scope, other := "states-user-"+stamp, "states-other-"+stamp
	org := &interfaces.Organisation{ID: "states-org-" + stamp, Name: "States", OwnerID: scope}
	if err := b.SaveOrg(ctx, org); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	t.Cleanup(func() {
		for _, s := range []string{scope, other} {
			_, _ = b.DB().ExecContext(context.Background(), `DELETE FROM provider_keys WHERE user_id = $1`, s)
		}
		_ = b.DeleteOrg(context.Background(), org.ID)
	})
	rotated, err := b.NewEncryptedKeyStore([]byte("a-different-deployment-secret-0987654321"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ks.Save(scope, "openai", "sk-own"))
	must(ks.Save(scope, "glm", "")) // a legacy empty row
	must(rotated.Save(scope, "gemini", "sealed-under-a-key-this-store-lacks"))
	must(ks.Save(scope, "xai", "not-asked-for"))
	must(ks.Save(other, "claude", "someone-elses"))
	must(ks.SaveOrg(org.ID, "openai", "org-sk", scope))
	must(rotated.SaveOrg(org.ID, "gemini", "org-rotated", scope))

	keys := []string{"openai", "glm", "gemini", "claude", "copilot-oauth-token"}
	got, err := ks.KeyStates(ctx, scope, keys)
	if err != nil {
		t.Fatalf("KeyStates: %v", err)
	}
	want := map[string]interfaces.SecretState{
		"openai": interfaces.SecretUsable,
		"glm":    interfaces.SecretEmpty,
		"gemini": interfaces.SecretUnreadable,
	}
	if !maps.Equal(got, want) {
		t.Errorf("KeyStates = %v, want %v", got, want)
	}
	// The states agree with what Get says of each key.
	for _, k := range keys {
		v, err := ks.Get(scope, k)
		var state interfaces.SecretState
		switch {
		case err == nil && v != "":
			state = interfaces.SecretUsable
		case err == nil:
			state = interfaces.SecretEmpty
		case errors.Is(err, storage.ErrSecretDecryptFailed):
			state = interfaces.SecretUnreadable
		}
		if got[k] != state {
			t.Errorf("%s: KeyStates says %v, Get (%q, %v) says %v", k, got[k], v, err, state)
		}
	}

	orgGot, err := ks.OrgKeyStates(ctx, org.ID, []string{"openai", "gemini", "glm"})
	if err != nil {
		t.Fatalf("OrgKeyStates: %v", err)
	}
	if want := map[string]interfaces.SecretState{"openai": interfaces.SecretUsable, "gemini": interfaces.SecretUnreadable}; !maps.Equal(orgGot, want) {
		t.Errorf("OrgKeyStates = %v, want %v", orgGot, want)
	}

	// No keys asked: no query, an empty answer.
	if got, err := ks.KeyStates(ctx, scope, nil); err != nil || len(got) != 0 {
		t.Errorf("KeyStates(no keys) = (%v, %v), want an empty map", got, err)
	}
}

// TestKeystoreReads_EndWithTheirContext: a read the database does not answer (the table is locked by another
// transaction) ends when its context does — not at the store's 5 s timeout — for the scope reads and the
// single-key ones alike.
func TestKeystoreReads_EndWithTheirContext(t *testing.T) {
	b, ks := openFamilyKeystore(t)
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	scope := "states-blocked-" + stamp
	org := &interfaces.Organisation{ID: "states-blocked-org-" + stamp, Name: "Blocked", OwnerID: scope}
	if err := b.SaveOrg(context.Background(), org); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), org.ID) })

	reads := map[string]func(context.Context) error{
		"KeyStates": func(ctx context.Context) error {
			_, err := ks.KeyStates(ctx, scope, []string{"openai"})
			return err
		},
		"OrgKeyStates": func(ctx context.Context) error {
			_, err := ks.OrgKeyStates(ctx, org.ID, []string{"openai"})
			return err
		},
		"GetCtx": func(ctx context.Context) error {
			_, err := ks.GetCtx(ctx, scope, "openai")
			return err
		},
		"HasCtx": func(ctx context.Context) error {
			_, err := ks.HasCtx(ctx, scope, "openai")
			return err
		},
		"GetOrgCtx": func(ctx context.Context) error {
			_, err := ks.GetOrgCtx(ctx, org.ID, "openai")
			return err
		},
		"HasOrgCtx": func(ctx context.Context) error {
			_, err := ks.HasOrgCtx(ctx, org.ID, "openai")
			return err
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			// Another session holds both tables exclusively: every read of them waits.
			lock, err := b.DB().BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Rollback() }()
			if _, err := lock.Exec(`LOCK TABLE provider_keys, org_provider_keys IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatalf("lock: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- read(ctx) }()
			time.Sleep(100 * time.Millisecond) // the read is waiting on the lock
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("the read answered while the table was locked")
				}
				if took := time.Since(start); took > 2*time.Second {
					t.Errorf("the read ended %v after it started, want soon after its context (the 5 s timeout is only a ceiling)", took)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the read outlived its context: still waiting 3 s after the cancel")
			}
		})
	}
}
