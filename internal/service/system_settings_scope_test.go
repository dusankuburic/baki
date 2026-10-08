package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// TestSystemService_EmptyUserScopeIsTheDeployment: without accounts the caller's key scope is empty, and the "user"
// settings are the deployment's, as on the desktop. The store keeps user settings under a user row the local user
// does not have, so local mode on a database answered every save with an error.
func TestSystemService_EmptyUserScopeIsTheDeployment(t *testing.T) {
	b := testutil.NewFakeBackend()
	deployment := deploymentSettings(nil)
	bus := eventbus.New(nil, 0)
	rec := eventbus.NewRecorder(bus)
	sys := NewSystemService(deployment, nil, NilNotifier{}, b, config.ModeLocal, nil, bus)
	ctx := context.Background()

	want := *models.DefaultSettings()
	want.Appearance.Theme = "light"
	if err := sys.UpdateUserSettings(ctx, "", want); err != nil {
		t.Fatalf("UpdateUserSettings without a user: %v", err)
	}
	if deployment.Get().Appearance.Theme != "light" {
		t.Errorf("deployment theme = %q, want light", deployment.Get().Appearance.Theme)
	}
	got, err := sys.GetUserSettings(ctx, "")
	if err != nil || got.Appearance.Theme != "light" {
		t.Errorf("GetUserSettings without a user: %v; want the light theme", err)
	}
	if changed := eventbus.Recorded[events.SettingsChanged](rec); len(changed) != 1 || changed[0] != (events.SettingsChanged{Scope: events.DeploymentSettings}) {
		t.Errorf("announced %+v, want one deployment settings change", changed)
	}
}
