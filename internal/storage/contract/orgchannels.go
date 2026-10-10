package contract

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// runOrgChannelScenarios holds the organisations' notification channels.
func runOrgChannelScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	// SaveOrgChannel is keyed on a client-supplied id while the HTTP layer authorizes against the org in
	// the URL, so "id wins" would let an admin of one org overwrite another org's channel — replacing its
	// webhook url and secret while the row keeps its owner. The upsert must be org-scoped and the rejection
	// VISIBLE (ErrNotFound), not a silent no-op. Backends without org support report that and are skipped.
	run("SaveOrgChannel_cannot_retarget_another_orgs_channel", func(t *testing.T) {
		victimOrg := "contract-org-victim-" + runID
		attackerOrg := "contract-org-attacker-" + runID
		channelID := "contract-channel-" + runID

		victim := &interfaces.OrgChannel{
			ID: channelID, OrgID: victimOrg, Name: "Victim Ops",
			Kind: "webhook", URL: "https://victim.example.com/hook",
			Secret: "victim-secret", Enabled: true, CreatedAt: time.Now().UTC(),
		}
		if err := b.SaveOrgChannel(ctx, victim); err != nil {
			t.Skipf("backend does not support org channels: %v", err)
		}

		attempt := &interfaces.OrgChannel{
			ID: channelID, OrgID: attackerOrg, Name: "Attacker",
			Kind: "webhook", URL: "https://attacker.example.com/collect",
			Secret: "attacker-secret", Enabled: true, CreatedAt: time.Now().UTC(),
		}
		err := b.SaveOrgChannel(ctx, attempt)
		if err == nil {
			t.Fatal("cross-org SaveOrgChannel succeeded — an org admin can retarget another org's notification channel")
		}
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("cross-org SaveOrgChannel: got %v, want ErrNotFound", err)
		}

		// The victim's row must be untouched — url and secret especially, since
		// those are what redirect the alert stream.
		list, err := b.ListOrgChannels(ctx, victimOrg, false)
		if err != nil {
			t.Fatalf("ListOrgChannels(victim): %v", err)
		}
		var found *interfaces.OrgChannel
		for _, c := range list {
			if c.ID == channelID {
				found = c
				break
			}
		}
		if found == nil {
			t.Fatal("victim channel disappeared")
		}
		if found.OrgID != victimOrg || found.URL != victim.URL || found.Secret != victim.Secret || found.Name != victim.Name {
			t.Errorf("victim channel was modified: %+v", found)
		}

		// The attacker must also not have gained a channel of their own.
		if attackerList, err := b.ListOrgChannels(ctx, attackerOrg, false); err != nil {
			t.Fatalf("ListOrgChannels(attacker): %v", err)
		} else if len(attackerList) != 0 {
			t.Errorf("attacker org gained %d channel(s) from a rejected save", len(attackerList))
		}

		// A same-org update of the same id is still allowed — the fix must not break the legitimate edit path.
		victim.URL = "https://victim.example.com/hook-v2"
		if err := b.SaveOrgChannel(ctx, victim); err != nil {
			t.Fatalf("same-org update of an existing channel should succeed: %v", err)
		}

		_ = b.DeleteOrgChannel(ctx, victimOrg, channelID)
	})

	// The URL (for Slack and Teams the credential) and the signing secret come back exactly as saved, an empty
	// secret as empty: the service keeps a secret an edit omits by re-saving the stored one.
	run("OrgChannel_secret_round_trip", func(t *testing.T) {
		org := "contract-org-secret-" + runID
		ch := &interfaces.OrgChannel{ID: "contract-ch-secret-" + runID, OrgID: org, Name: "ops", Kind: "slack",
			URL: "https://hooks.slack.com/services/T0/B0/" + runID, Secret: "s3cret-" + runID, Enabled: true,
			CreatedAt: time.Now().UTC()}
		if err := b.SaveOrgChannel(ctx, ch); err != nil {
			t.Skipf("backend does not support org channels: %v", err)
		}
		defer func() { _ = b.DeleteOrgChannel(ctx, org, ch.ID) }()
		got := findOrgChannel(t, ctx, b, org, ch.ID, false)
		if got == nil || got.URL != ch.URL || got.Secret != ch.Secret || got.Kind != "slack" || got.Name != "ops" {
			t.Fatalf("stored channel = %+v, want url, secret, kind and name as saved", got)
		}
		ch.Secret = ""
		if err := b.SaveOrgChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
		if got := findOrgChannel(t, ctx, b, org, ch.ID, false); got == nil || got.Secret != "" || got.URL != ch.URL {
			t.Errorf("after clearing the secret: %+v, want an empty secret and the url kept", got)
		}
	})

	// enabledOnly is the scanner's read: a paused channel is listed for the settings page and never delivered to.
	run("ListOrgChannels_enabledOnly_skips_paused", func(t *testing.T) {
		org := "contract-org-paused-" + runID
		on := &interfaces.OrgChannel{ID: "contract-ch-on-" + runID, OrgID: org, Name: "on", Kind: "webhook",
			URL: "https://hooks.example.com/on", Enabled: true, CreatedAt: time.Now().UTC()}
		off := &interfaces.OrgChannel{ID: "contract-ch-off-" + runID, OrgID: org, Name: "off", Kind: "webhook",
			URL: "https://hooks.example.com/off", Enabled: false, CreatedAt: time.Now().UTC()}
		for _, ch := range []*interfaces.OrgChannel{on, off} {
			if err := b.SaveOrgChannel(ctx, ch); err != nil {
				t.Skipf("backend does not support org channels: %v", err)
			}
			defer func(id string) { _ = b.DeleteOrgChannel(ctx, org, id) }(ch.ID)
		}
		all, err := b.ListOrgChannels(ctx, org, false)
		if err != nil || len(all) != 2 {
			t.Fatalf("ListOrgChannels(all) = %d, %v; want both", len(all), err)
		}
		enabled, err := b.ListOrgChannels(ctx, org, true)
		if err != nil || len(enabled) != 1 || enabled[0].ID != on.ID {
			t.Errorf("ListOrgChannels(enabledOnly) = %+v, %v; want only %s", enabled, err, on.ID)
		}
	})

	// An update keeps the row's creation time (the list is ordered by it): Postgres never sets created_at on
	// conflict, and the fake must not take the caller's either (B9).
	run("SaveOrgChannel_update_keeps_created_at", func(t *testing.T) {
		org := "contract-org-created-" + runID
		first := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
		ch := &interfaces.OrgChannel{ID: "contract-ch-created-" + runID, OrgID: org, Name: "ops", Kind: "webhook",
			URL: "https://hooks.example.com/x", Enabled: true, CreatedAt: first}
		if err := b.SaveOrgChannel(ctx, ch); err != nil {
			t.Skipf("backend does not support org channels: %v", err)
		}
		defer func() { _ = b.DeleteOrgChannel(ctx, org, ch.ID) }()
		update := *ch
		update.Name, update.Enabled, update.CreatedAt = "renamed", false, time.Now().UTC()
		if err := b.SaveOrgChannel(ctx, &update); err != nil {
			t.Fatal(err)
		}
		got := findOrgChannel(t, ctx, b, org, ch.ID, false)
		if got == nil || got.Name != "renamed" || got.Enabled {
			t.Fatalf("after the update: %+v, want the new name, paused", got)
		}
		if !got.CreatedAt.Equal(first) {
			t.Errorf("created_at = %v after an update, want %v kept", got.CreatedAt, first)
		}
	})

	// A kind's fields beyond url and secret (jira's) and the event subscription come back as saved, and a save writes
	// what it is given: the service, not the store, keeps what an edit omits. No events is the default set (nil), and
	// what a caller changes after a save or a read never reaches the store.
	run("OrgChannel_fields_and_events_round_trip", func(t *testing.T) {
		org := "contract-org-fields-" + runID
		ch := &interfaces.OrgChannel{ID: "contract-ch-fields-" + runID, OrgID: org, Name: "jira", Kind: "jira",
			URL: "https://acme.atlassian.net", Config: map[string]string{"email": "bot@acme.io", "project": "PAD"},
			Secrets: map[string]string{"token": "tok-" + runID}, Events: []string{"drift", "new_rules"}, Enabled: true,
			CreatedAt: time.Now().UTC()}
		if err := b.SaveOrgChannel(ctx, ch); err != nil {
			t.Skipf("backend does not support org channels: %v", err)
		}
		defer func() { _ = b.DeleteOrgChannel(ctx, org, ch.ID) }()
		ch.Config["project"], ch.Secrets["token"], ch.Events[0] = "CHANGED", "changed", "changed"
		got := findOrgChannel(t, ctx, b, org, ch.ID, false)
		if got == nil || got.Unreadable || got.Config["email"] != "bot@acme.io" || got.Config["project"] != "PAD" ||
			got.Secrets["token"] != "tok-"+runID || !slices.Equal(got.Events, []string{"drift", "new_rules"}) {
			t.Fatalf("stored channel = %+v, want config, secrets and events as saved", got)
		}
		got.Config["project"] = "CHANGED"
		if again := findOrgChannel(t, ctx, b, org, ch.ID, false); again.Config["project"] != "PAD" {
			t.Errorf("a change to a read channel reached the store: %+v", again)
		}
		bare := &interfaces.OrgChannel{ID: ch.ID, OrgID: org, Name: "jira", Kind: "jira", URL: ch.URL, Enabled: true, CreatedAt: ch.CreatedAt}
		if err := b.SaveOrgChannel(ctx, bare); err != nil {
			t.Fatal(err)
		}
		if got := findOrgChannel(t, ctx, b, org, ch.ID, false); got == nil || len(got.Config) != 0 || len(got.Secrets) != 0 || got.Events != nil {
			t.Errorf("after a save without fields: %+v, want no config, no secrets, the default events", got)
		}
	})

	// An email channel's recipients are member ids: erasing a user's account takes the user out of every list (the
	// organisation's other recipients stay), whether or not the user is still a member.
	run("DeleteUser_leaves_email_channel_recipients", func(t *testing.T) {
		org := "contract-org-recipients-" + runID
		gone, stays := "contract-rcpt-gone-"+runID, "contract-rcpt-stays-"+runID
		for _, id := range []string{gone, stays} {
			if err := b.CreateUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "hash"}); err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
		}
		defer func() { _ = b.DeleteUser(context.Background(), stays) }()
		ch := &interfaces.OrgChannel{ID: "contract-ch-rcpt-" + runID, OrgID: org, Name: "mail", Kind: "email",
			Config: map[string]string{"recipients": stays + "," + gone}, Enabled: true, CreatedAt: time.Now().UTC()}
		other := &interfaces.OrgChannel{ID: "contract-ch-rcpt-only-" + runID, OrgID: org, Name: "mail2", Kind: "email",
			Config: map[string]string{"recipients": gone}, Enabled: true, CreatedAt: time.Now().UTC()}
		for _, c := range []*interfaces.OrgChannel{ch, other} {
			if err := b.SaveOrgChannel(ctx, c); err != nil {
				t.Skipf("backend does not support org channels: %v", err)
			}
			defer func(id string) { _ = b.DeleteOrgChannel(ctx, org, id) }(c.ID)
		}
		if err := b.DeleteUser(ctx, gone); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		if got := findOrgChannel(t, ctx, b, org, ch.ID, false); got == nil || got.Config["recipients"] != stays {
			t.Errorf("after erasing %s: recipients %+v, want only %s", gone, got, stays)
		}
		if got := findOrgChannel(t, ctx, b, org, other.ID, false); got == nil || got.Config["recipients"] != "" {
			t.Errorf("after erasing the only recipient: %+v, want none left", got)
		}
	})
}

// findOrgChannel returns org's channel id, or nil.
func findOrgChannel(t *testing.T, ctx context.Context, b interfaces.StorageBackend, org, id string, enabledOnly bool) *interfaces.OrgChannel {
	t.Helper()
	list, err := b.ListOrgChannels(ctx, org, enabledOnly)
	if err != nil {
		t.Fatalf("ListOrgChannels: %v", err)
	}
	for _, c := range list {
		if c.ID == id {
			return c
		}
	}
	return nil
}
