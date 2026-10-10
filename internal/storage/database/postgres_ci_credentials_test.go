package database

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// Organisation CI credentials (migration 0038) beyond the contract: the secret is sealed at rest and never written
// without the key, and the org-member policy scopes the admin routes' statements. DATABASE_URL-gated.

func newCICredential(f *r2Fixture, label, orgID, createdBy string) *interfaces.CICredential {
	return &interfaces.CICredential{ID: f.id("ci-" + label), OrgID: orgID, Name: label,
		Secret: "baki_ci_" + label + "_s3cr3t-" + f.run, SecretHint: "hint", CreatedBy: createdBy}
}

func TestPostgres_CICredentials_SealedAtRest(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 38 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	admin := f.user("ci-sealed-admin")
	org := f.org("ci-sealed", admin, map[string]string{admin: "admin"})

	plain := newCICredential(f, "plain", org, admin)
	if err := f.b.CreateCICredential(f.ctx, plain, 10); !errors.Is(err, interfaces.ErrSecretSealingUnavailable) {
		t.Fatalf("CreateCICredential without a field key = %v, want ErrSecretSealingUnavailable", err)
	}
	var n int
	if err := f.b.db.QueryRowContext(f.ctx, `SELECT count(*) FROM org_ci_credentials WHERE org_id = $1`, org).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d row(s) stored without a key (%v): a secret must never be stored in plaintext", n, err)
	}

	if err := f.b.EnableFieldEncryption([]byte(testFieldKey)); err != nil {
		t.Fatal(err)
	}
	c := newCICredential(f, "sealed", org, admin)
	if err := f.b.CreateCICredential(f.ctx, c, 10); err != nil {
		t.Fatalf("CreateCICredential: %v", err)
	}
	var raw string
	if err := f.b.db.QueryRowContext(f.ctx, `SELECT secret FROM org_ci_credentials WHERE id = $1`, c.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, sealedPrefix) || strings.Contains(raw, "s3cr3t") {
		t.Errorf("stored secret %q is not sealed", raw)
	}
	got, err := f.b.GetCICredential(f.ctx, c.ID)
	if err != nil || got.Secret != c.Secret {
		t.Errorf("GetCICredential = %+v, %v; want the opened secret", got, err)
	}
	// The ciphertext is bound to its row: copied onto another credential it does not open.
	other := newCICredential(f, "other", org, admin)
	if err := f.b.CreateCICredential(f.ctx, other, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.db.ExecContext(f.ctx, `UPDATE org_ci_credentials SET secret = $1 WHERE id = $2`, raw, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.GetCICredential(f.ctx, other.ID); err == nil {
		t.Error("a sealed secret copied to another row opened")
	}

	// A process without the key cannot read the secret, and says so rather than handing out ciphertext.
	keyless := openMigrationTestDB(t)
	if _, err := keyless.GetCICredential(f.ctx, c.ID); !errors.Is(err, ErrFieldKeyMissing) {
		t.Errorf("GetCICredential without the key = %v, want ErrFieldKeyMissing", err)
	}
	if list, err := keyless.ListCICredentials(f.ctx, org); err != nil || len(list) != 2 {
		t.Errorf("ListCICredentials without the key = %d rows, %v; the metadata list needs no secret", len(list), err)
	}
}

// TestRLS_CICredentials_MembersOnly: inside a user's request scope only the organisation's members reach its
// credentials, as with its channels; the webhook's pool reads see every row.
func TestRLS_CICredentials_MembersOnly(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 38 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	skipIfRLSBypassedInternal(t, f.b)
	if err := f.b.EnableFieldEncryption([]byte(testFieldKey)); err != nil {
		t.Fatal(err)
	}
	s := &scopedFixture{r2Fixture: f}
	admin, stranger := f.user("ci-rls-admin"), f.user("ci-rls-stranger")
	org := f.org("ci-rls", admin, map[string]string{admin: "admin"})
	strangerOrg := f.org("ci-rls-other", stranger, map[string]string{stranger: "admin"})
	c := newCICredential(f, "rls", org, admin)
	s.as(admin, func(ctx context.Context) {
		if err := f.b.CreateCICredential(ctx, c, 10); err != nil {
			t.Fatalf("CreateCICredential in the admin's scope: %v", err)
		}
	})

	s.as(stranger, func(ctx context.Context) {
		if list, err := f.b.ListCICredentials(ctx, org); err != nil || len(list) != 0 {
			t.Errorf("stranger ListCICredentials = %d rows, %v; want none", len(list), err)
		}
		if _, err := f.b.GetCICredential(ctx, c.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("stranger GetCICredential = %v, want ErrNotFound", err)
		}
		if err := f.b.DeleteCICredential(ctx, org, c.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("stranger DeleteCICredential = %v, want ErrNotFound", err)
		}
	})
	// A refused write aborts its transaction, so it gets one of its own.
	s.as(stranger, func(ctx context.Context) {
		if err := f.b.CreateCICredential(ctx, newCICredential(f, "planted", org, stranger), 10); err == nil {
			t.Error("a stranger planted a credential in another organisation")
		}
	})
	s.as(stranger, func(ctx context.Context) {
		if err := f.b.CreateCICredential(ctx, newCICredential(f, "own", strangerOrg, stranger), 10); err != nil {
			t.Errorf("a stranger's credential in their own organisation: %v", err)
		}
	})

	if n := s.rows("org_ci_credentials", "org_id = $1", org); n != 1 {
		t.Errorf("%d credential(s) in the organisation after the stranger's writes, want 1", n)
	}
	s.as(admin, func(ctx context.Context) {
		if list, err := f.b.ListCICredentials(ctx, org); err != nil || len(list) != 1 {
			t.Errorf("member ListCICredentials = %d rows, %v; want 1", len(list), err)
		}
	})
	if got, err := f.b.GetCICredential(f.ctx, c.ID); err != nil || got.Secret != c.Secret {
		t.Errorf("pool GetCICredential = %+v, %v", got, err)
	}
	s.as(admin, func(ctx context.Context) {
		if err := f.b.DeleteCICredential(ctx, org, c.ID); err != nil {
			t.Errorf("member DeleteCICredential: %v", err)
		}
	})
}

// TestPurgeExpiredData_CICredentials: the retention sweep removes credentials expired more than 30 days ago and
// keeps the rest.
func TestPurgeExpiredData_CICredentials(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	f := &r2Fixture{t: t, b: b, ctx: context.Background(), run: r2stamp()}
	if err := b.EnableFieldEncryption([]byte(testFieldKey)); err != nil {
		t.Fatal(err)
	}
	admin := f.user("ci-purge-admin")
	org := f.org("ci-purge", admin, map[string]string{admin: "admin"})
	long, recent := time.Now().Add(-31*24*time.Hour), time.Now().Add(-24*time.Hour)
	for label, exp := range map[string]*time.Time{"long": &long, "recent": &recent, "never": nil} {
		c := newCICredential(f, label, org, admin)
		c.ExpiresAt = exp
		if err := b.CreateCICredential(f.ctx, c, 10); err != nil {
			t.Fatalf("CreateCICredential %s: %v", label, err)
		}
	}
	// A use older than the five-minute throttle is recorded again.
	never := f.id("ci-never")
	if _, err := b.db.ExecContext(f.ctx, `UPDATE org_ci_credentials SET last_used_at = NOW() - INTERVAL '6 minutes' WHERE id = $1`, never); err != nil {
		t.Fatal(err)
	}
	before, _ := b.GetCICredential(f.ctx, never)
	if err := b.TouchCICredential(f.ctx, never); err != nil {
		t.Fatal(err)
	}
	if after, _ := b.GetCICredential(f.ctx, never); after.LastUsedAt == nil || before.LastUsedAt == nil || !after.LastUsedAt.After(*before.LastUsedAt) {
		t.Errorf("a use six minutes old was not refreshed: %v, then %v", before.LastUsedAt, after.LastUsedAt)
	}

	res, err := b.PurgeExpiredData(f.ctx, 0)
	if err != nil {
		t.Fatalf("PurgeExpiredData: %v", err)
	}
	if res.CICredentials != 1 {
		t.Errorf("purged %d CI credentials, want 1", res.CICredentials)
	}
	list, err := b.ListCICredentials(f.ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range list {
		names = append(names, c.Name)
	}
	if len(names) != 2 || slices.Contains(names, "long") {
		t.Errorf("left %v; want the recently expired and the unexpiring credential", names)
	}
}
