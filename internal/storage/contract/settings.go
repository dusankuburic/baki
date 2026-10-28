package contract

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// runSettingsScenarios holds the user and organisation settings round trips.
func runSettingsScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("SaveUserSettings_then_LoadUserSettings_round_trips", func(t *testing.T) {
		userID := "contract-usettings-" + runID
		// Seed the user first: Postgres enforces the user_settings FK (seeding keeps the test valid on both backends).
		if err := b.CreateUser(ctx, &interfaces.User{
			ID: userID, Email: "usettings-" + runID + "@example.com", Password: "h",
		}); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		settings := &interfaces.AppSettings{
			Version:    1,
			Appearance: interfaces.AppearanceSettings{Density: "compact"},
		}
		if err := b.SaveUserSettings(ctx, userID, settings); err != nil {
			t.Fatalf("SaveUserSettings: %v", err)
		}
		got, err := b.LoadUserSettings(ctx, userID)
		if err != nil {
			t.Fatalf("LoadUserSettings: %v", err)
		}
		if got.Appearance.Density != "compact" {
			t.Errorf("round-trip: want compact, got %s", got.Appearance.Density)
		}
	})

	run("SaveOrgSettings_then_LoadOrgSettings_round_trips", func(t *testing.T) {
		orgID := "contract-org-" + runID
		// Seed the org first: Postgres enforces org_settings.org_id → organisations(id).
		if err := b.SaveOrg(ctx, &interfaces.Organisation{
			ID: orgID, Name: "Contract Org", OwnerID: "contract-owner-" + runID,
		}); err != nil {
			t.Fatalf("seed org: %v", err)
		}
		settings := &interfaces.AppSettings{
			Version: 1,
			Layout:  interfaces.LayoutSettings{LastActiveInspectorTab: "findings"},
		}
		if err := b.SaveOrgSettings(ctx, orgID, settings); err != nil {
			t.Fatalf("SaveOrgSettings: %v", err)
		}
		got, err := b.LoadOrgSettings(ctx, orgID)
		if err != nil {
			t.Fatalf("LoadOrgSettings: %v", err)
		}
		if got.Layout.LastActiveInspectorTab != "findings" {
			t.Errorf("round-trip: want findings, got %s", got.Layout.LastActiveInspectorTab)
		}
	})

	// R-config-4: a section-level write reads the stored document under the settings rows' lock and writes in the
	// same transaction, so a second writer reads only after the first committed and both changes land, even to
	// different fields of one section (which a whole-section replace would lose).
	run("UpdateOrgSettingsWith_serialises_concurrent_writers", func(t *testing.T) {
		orgID := "contract-org-rmw-" + runID
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "RMW Org", OwnerID: "contract-owner-" + runID}); err != nil {
			t.Fatalf("seed org: %v", err)
		}
		if err := b.SaveOrgSettings(ctx, orgID, &interfaces.AppSettings{Version: 1, AI: interfaces.AISettings{SystemPromptSuffix: "old"}}); err != nil {
			t.Fatalf("SaveOrgSettings: %v", err)
		}
		inFirst, release := make(chan struct{}), make(chan struct{})
		secondRead := make(chan struct{}, 1)
		errs := make(chan error, 2)
		go func() {
			errs <- b.UpdateOrgSettingsWith(ctx, orgID, func(s *interfaces.AppSettings) error {
				close(inFirst)
				<-release
				s.AI.AllowedProviders = []string{"claude"}
				return nil
			})
		}()
		<-inFirst
		go func() {
			errs <- b.UpdateOrgSettingsWith(ctx, orgID, func(s *interfaces.AppSettings) error {
				secondRead <- struct{}{}
				s.AI.SystemPromptSuffix = "new"
				return nil
			})
		}()
		select {
		case <-secondRead:
			t.Error("the second writer read the document while the first still held it")
		case <-time.After(300 * time.Millisecond):
		}
		close(release)
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("UpdateOrgSettingsWith: %v", err)
			}
		}
		got, err := b.LoadOrgSettings(ctx, orgID)
		if err != nil {
			t.Fatalf("LoadOrgSettings: %v", err)
		}
		if !slices.Equal(got.AI.AllowedProviders, []string{"claude"}) || got.AI.SystemPromptSuffix != "new" {
			t.Errorf("after two concurrent writers: allowedProviders %v, systemPromptSuffix %q; want [claude] and \"new\" (neither reverts)",
				got.AI.AllowedProviders, got.AI.SystemPromptSuffix)
		}

		// A mutate that fails writes nothing.
		boom := errors.New("refused")
		if err := b.UpdateOrgSettingsWith(ctx, orgID, func(s *interfaces.AppSettings) error {
			s.AI.SystemPromptSuffix = "lost"
			return boom
		}); !errors.Is(err, boom) {
			t.Errorf("a refusing mutate: %v, want its error", err)
		}
		if got, err := b.LoadOrgSettings(ctx, orgID); err != nil || got.AI.SystemPromptSuffix != "new" {
			t.Errorf("after a refusing mutate: %+v, %v; want the document unchanged", got.AI, err)
		}
	})
}
