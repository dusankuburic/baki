package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

const testFieldKey = "test-field-encryption-key-0123456789abcdef"

func rawChannelColumns(t *testing.T, b *PostgresStorageBackend, id string) (url, secret string) {
	t.Helper()
	if err := b.db.QueryRowContext(context.Background(), `SELECT url, secret FROM org_channels WHERE id = $1`, id).Scan(&url, &secret); err != nil {
		t.Fatalf("read raw channel %s: %v", id, err)
	}
	return url, secret
}

// TestOrgChannels_EncryptedAtRest (S3): with field encryption on, the webhook URL (for Slack and Teams, the
// credential) and the signing secret are stored as ciphertext and read back as plaintext. The ciphertext is
// bound to its row and column, and a process without the key refuses to read it instead of returning garbage.
func TestOrgChannels_EncryptedAtRest(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if err := b.EnableFieldEncryption([]byte(testFieldKey)); err != nil {
		t.Fatal(err)
	}
	seedOrgRow(t, b.db, "org-fc")
	const url, secret = "https://hooks.slack.com/services/T000/B000/XXXXSECRET", "hmac-signing-secret"
	for _, id := range []string{"ch-1", "ch-2"} {
		if err := b.SaveOrgChannel(ctx, &storageif.OrgChannel{
			ID: id, OrgID: "org-fc", Name: id, Kind: "slack", URL: url, Secret: secret, Enabled: true, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}

	rawURL, rawSecret := rawChannelColumns(t, b, "ch-1")
	if !strings.HasPrefix(rawURL, sealedPrefix) || !strings.HasPrefix(rawSecret, sealedPrefix) ||
		strings.Contains(rawURL, "XXXXSECRET") || strings.Contains(rawSecret, secret) {
		t.Fatalf("stored in plaintext: url %q secret %q", rawURL, rawSecret)
	}

	got, err := b.ListOrgChannels(ctx, "org-fc", false)
	if err != nil || len(got) != 2 {
		t.Fatalf("list: %v (%d channels)", err, len(got))
	}
	for _, ch := range got {
		if ch.URL != url || ch.Secret != secret {
			t.Errorf("%s read back url %q secret %q", ch.ID, ch.URL, ch.Secret)
		}
	}

	// A ciphertext copied onto another row does not decrypt there.
	if _, err := b.db.ExecContext(ctx, `UPDATE org_channels SET url = $1 WHERE id = 'ch-2'`, rawURL); err != nil {
		t.Fatal(err)
	}
	moved, err := b.ListOrgChannels(ctx, "org-fc", false)
	if err != nil || len(moved) != 2 {
		t.Fatalf("list after moving a ciphertext: %d channels, %v", len(moved), err)
	}
	for _, ch := range moved {
		if ch.Unreadable != (ch.ID == "ch-2") || (ch.ID == "ch-2" && ch.URL != "") {
			t.Errorf("a ciphertext moved to ch-2: %s listed %+v; want only ch-2 unreadable, without a URL", ch.ID, ch)
		}
	}
	if _, err := b.db.ExecContext(ctx, `DELETE FROM org_channels WHERE id = 'ch-2'`); err != nil {
		t.Fatal(err)
	}

	// A process without the key refuses rather than returning ciphertext as a URL: the channel is listed unreadable.
	keyless, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer keyless.Close()
	if got, err := keyless.ListOrgChannels(ctx, "org-fc", false); err != nil || len(got) != 1 || !got[0].Unreadable || got[0].URL != "" || got[0].Secret != "" {
		t.Errorf("keyless read: %+v, %v; want the channel unreadable, without its values", got, err)
	}
	if _, err := keyless.openField("org_channels", "url", "ch-1", rawURL); !errors.Is(err, ErrFieldKeyMissing) {
		t.Errorf("keyless open: err = %v, want ErrFieldKeyMissing", err)
	}
}

// TestEncryptOrgChannelsAtRest_SealsLegacyRowsOnce: rows written in plaintext before encryption was enabled stay
// readable, and the backfill seals them, once. An empty secret stays empty, and a row with nothing to seal (an empty
// url, as a kind without a URL would store) is not counted, so it is not rewritten on every boot (B11).
func TestEncryptOrgChannelsAtRest_SealsLegacyRowsOnce(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	seedOrgRow(t, b.db, "org-bf")
	seedOrgRow(t, b.db, "org-bf-empty")
	if _, err := b.db.ExecContext(ctx, `INSERT INTO org_channels (id, org_id, name, kind, url, secret, created_at) VALUES
		('legacy-1', 'org-bf', 'a', 'webhook', 'https://hooks.example.com/a', 's3cret', NOW() - INTERVAL '1 minute'),
		('legacy-2', 'org-bf', 'b', 'teams',   'https://acme.webhook.office.com/b', '', NOW()),
		('empty-3',  'org-bf-empty', 'c', 'future-kind', '', '', NOW())`); err != nil {
		t.Fatal(err)
	}
	if err := b.EnableFieldEncryption([]byte(testFieldKey)); err != nil {
		t.Fatal(err)
	}
	if got, err := b.ListOrgChannels(ctx, "org-bf", false); err != nil || len(got) != 2 || got[0].URL != "https://hooks.example.com/a" {
		t.Fatalf("legacy plaintext rows must stay readable: %v %+v", err, got)
	}

	n, err := b.EncryptOrgChannelsAtRest(ctx)
	if err != nil || n != 2 {
		t.Fatalf("backfill sealed %d rows (err %v), want 2", n, err)
	}
	u1, s1 := rawChannelColumns(t, b, "legacy-1")
	u2, s2 := rawChannelColumns(t, b, "legacy-2")
	if !strings.HasPrefix(u1, sealedPrefix) || !strings.HasPrefix(s1, sealedPrefix) || !strings.HasPrefix(u2, sealedPrefix) || s2 != "" {
		t.Fatalf("after backfill: legacy-1 %q %q, legacy-2 %q %q", u1, s1, u2, s2)
	}
	got, err := b.ListOrgChannels(ctx, "org-bf", false)
	if err != nil || got[0].URL != "https://hooks.example.com/a" || got[0].Secret != "s3cret" || got[1].Secret != "" {
		t.Fatalf("read after backfill: %v %+v", err, got)
	}
	if n, err := b.EncryptOrgChannelsAtRest(ctx); err != nil || n != 0 {
		t.Errorf("second backfill sealed %d rows (err %v), want 0", n, err)
	}
}
