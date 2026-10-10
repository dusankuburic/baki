package database

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// TestOrgChannels_UnreadableRowIsListedNotFatal: one channel sealed with a key this process does not have (the
// encryption key changed without listing the old one in PAD_ENCRYPTION_KEY_PREVIOUS) used to fail its organisation's
// whole list, so the admin could neither see nor delete it and the scanner delivered to none of the org's channels.
// It is listed unreadable, without a value, the other channels as they are; it can be deleted, and re-saved with a
// new URL it opens again.
func TestOrgChannels_UnreadableRowIsListedNotFatal(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if err := b.EnableFieldEncryption([]byte(testFieldKey)); err != nil {
		t.Fatal(err)
	}
	other := rotationReplica(t, dsn, "another-field-encryption-key-0123456789abcdef")
	save := func(b *PostgresStorageBackend, id, url string, created time.Time) {
		t.Helper()
		if err := b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: id, OrgID: "org-ur", Name: id, Kind: "jira", URL: url,
			Secret: "s-" + id, Secrets: map[string]string{"token": "tok-" + id}, Config: map[string]string{"project": "PAD"},
			Enabled: true, CreatedAt: created}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	now := time.Now().UTC()
	save(b, "ok-1", "https://acme.atlassian.net", now.Add(-2*time.Minute))
	save(other, "lost-2", "https://lost.atlassian.net/XXXXSECRET", now.Add(-time.Minute))
	save(b, "ok-3", "https://acme.atlassian.net", now)

	list, err := b.ListOrgChannels(ctx, "org-ur", false)
	if err != nil || len(list) != 3 {
		t.Fatalf("ListOrgChannels = %d channels, %v; want all three listed", len(list), err)
	}
	for _, ch := range list {
		lost := ch.ID == "lost-2"
		if ch.Unreadable != lost {
			t.Errorf("%s: unreadable %v, want %v", ch.ID, ch.Unreadable, lost)
		}
		if lost {
			if ch.URL != "" || ch.Secret != "" || len(ch.Secrets) != 0 || ch.Name != "lost-2" || ch.Kind != "jira" || !ch.Enabled ||
				ch.Config["project"] != "PAD" {
				t.Errorf("unreadable row = %+v, want its id, name, kind, state and public config without a sealed value", ch)
			}
			continue
		}
		if ch.URL != "https://acme.atlassian.net" || ch.Secret != "s-"+ch.ID || ch.Secrets["token"] != "tok-"+ch.ID {
			t.Errorf("%s read back %+v", ch.ID, ch)
		}
	}
	if enabled, err := b.ListOrgChannels(ctx, "org-ur", true); err != nil || len(enabled) != 3 {
		t.Errorf("ListOrgChannels(enabledOnly) = %d, %v; want the readable ones delivered beside the unreadable one", len(enabled), err)
	}

	// Re-saved with a new URL under this process's key, it opens again.
	save(b, "lost-2", "https://new.atlassian.net", now)
	list, err = b.ListOrgChannels(ctx, "org-ur", false)
	if err != nil || slices.ContainsFunc(list, func(c *storageif.OrgChannel) bool { return c.Unreadable }) {
		t.Fatalf("after the re-save: %+v, %v", list, err)
	}
	// And it can be deleted while unreadable.
	save(other, "lost-4", "https://lost.atlassian.net/x", now)
	if err := b.DeleteOrgChannel(ctx, "org-ur", "lost-4"); err != nil {
		t.Fatal(err)
	}
	if list, _ := b.ListOrgChannels(ctx, "org-ur", false); len(list) != 3 {
		t.Errorf("after deleting the unreadable row: %d channels, want 3", len(list))
	}

	// A process without any key lists the sealed rows unreadable rather than returning ciphertext as a URL.
	keyless, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer keyless.Close()
	list, err = keyless.ListOrgChannels(ctx, "org-ur", false)
	if err != nil || len(list) != 3 {
		t.Fatalf("keyless list = %d, %v", len(list), err)
	}
	for _, ch := range list {
		if !ch.Unreadable || strings.HasPrefix(ch.URL, sealedPrefix) || ch.URL != "" {
			t.Errorf("keyless read of %s = %+v, want it unreadable without a value", ch.ID, ch)
		}
	}
}

// TestOrgChannels_FieldsRoundTripSealedAndResealed: a kind's fields beyond url and secret round-trip (config public,
// secrets sealed as one value bound to its row and column, events as chosen or NULL for the default set); the
// at-rest backfill seals a secrets value written without a key, and a key rotation's re-seal pass moves it like url
// and secret.
func TestOrgChannels_FieldsRoundTripSealedAndResealed(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	jira := &storageif.OrgChannel{ID: "fld-1", OrgID: "org-fld", Name: "jira", Kind: "jira", URL: "https://acme.atlassian.net",
		Config: map[string]string{"email": "bot@acme.io", "project": "PAD"}, Secrets: map[string]string{"token": "XXXXTOKEN"},
		Events: []string{"drift"}, Enabled: true, CreatedAt: time.Now().UTC().Add(-time.Minute)}
	hook := &storageif.OrgChannel{ID: "fld-2", OrgID: "org-fld", Name: "hook", Kind: "webhook", URL: "https://hooks.example.com/x",
		Enabled: true, CreatedAt: time.Now().UTC()}
	for _, ch := range []*storageif.OrgChannel{jira, hook} {
		if err := b.SaveOrgChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	rawSecrets := func(id string) (secrets string, events *string) {
		t.Helper()
		if err := b.db.QueryRowContext(ctx, `SELECT secrets, events::text FROM org_channels WHERE id = $1`, id).Scan(&secrets, &events); err != nil {
			t.Fatal(err)
		}
		return secrets, events
	}
	if s, ev := rawSecrets("fld-2"); s != "" || ev != nil {
		t.Errorf("a channel without secrets or a subscription stored secrets %q, events %v; want '' and NULL", s, ev)
	}
	if s, _ := rawSecrets("fld-1"); s != `{"token":"XXXXTOKEN"}` {
		t.Fatalf("without a key the secrets are stored as given: %q", s)
	}

	if err := b.EnableFieldEncryption([]byte(rotOldKey)); err != nil {
		t.Fatal(err)
	}
	if n, err := b.EncryptOrgChannelsAtRest(ctx); err != nil || n != 2 {
		t.Fatalf("backfill sealed %d rows, %v; want both", n, err)
	}
	if s, _ := rawSecrets("fld-1"); !strings.HasPrefix(s, sealedPrefix) || strings.Contains(s, "XXXXTOKEN") {
		t.Fatalf("after the backfill the secrets are stored %q", s)
	}
	if n, err := b.EncryptOrgChannelsAtRest(ctx); err != nil || n != 0 {
		t.Errorf("second backfill sealed %d rows, %v; want none", n, err)
	}

	rotated := rotationReplica(t, dsn, rotNewKey, rotOldKey)
	rep, err := rotated.ResealFields(ctx)
	if err != nil || rep.Resealed != 3 || rep.Unreadable != 0 {
		t.Fatalf("re-seal = %+v, %v; want the two urls and the secrets moved", rep, err)
	}
	list, err := rotationReplica(t, dsn, rotNewKey).ListOrgChannels(ctx, "org-fld", false)
	if err != nil || len(list) != 2 {
		t.Fatalf("list with the new key alone: %d, %v", len(list), err)
	}
	got := list[0]
	if got.Unreadable || got.Secrets["token"] != "XXXXTOKEN" || got.Config["email"] != "bot@acme.io" || got.Config["project"] != "PAD" ||
		!slices.Equal(got.Events, []string{"drift"}) {
		t.Errorf("jira channel read back %+v", got)
	}
	if list[1].Events != nil || len(list[1].Config) != 0 || len(list[1].Secrets) != 0 {
		t.Errorf("webhook channel read back %+v; want no events (the default set), config or secrets", list[1])
	}

	// A secrets value copied onto another row does not open there.
	s, _ := rawSecrets("fld-1")
	if _, err := b.db.ExecContext(ctx, `UPDATE org_channels SET secrets = $1 WHERE id = 'fld-2'`, s); err != nil {
		t.Fatal(err)
	}
	if list, err := rotated.ListOrgChannels(ctx, "org-fld", false); err != nil || !list[1].Unreadable || list[0].Unreadable {
		t.Errorf("a moved secrets value: %+v, %v; want only that row unreadable", list, err)
	}
}
