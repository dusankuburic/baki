package contract

import (
	"context"
	"testing"

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
}
