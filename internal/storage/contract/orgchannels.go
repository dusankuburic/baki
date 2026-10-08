package contract

import (
	"context"
	"errors"
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
}
