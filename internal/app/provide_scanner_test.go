package app

import (
	"testing"

	"pad-analyzer/internal/config"
)

// TestProvideScanner_NoticesOfADeploymentWithoutAccountsAreTheDeployments: without accounts every flow is owned by
// "local", which has no users row, so a user-scoped "new rules" notice would be skipped by the store and never shown;
// the scanner must put them in the deployment's scope. With accounts they go to the flow's org or owner.
func TestProvideScanner_NoticesOfADeploymentWithoutAccountsAreTheDeployments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   config.DeploymentMode
		auth   bool
		single bool
	}{
		{"local+db without auth", config.ModeLocal, false, true},
		{"local+db with auth", config.ModeLocal, true, false},
		{"cloud", config.ModeCloud, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Mode: tc.mode}
			cfg.Storage.Backend = config.StorageDatabase
			cfg.Auth.Enabled = tc.auth
			s := provideScanner(cfg, nil, nil, nil, nil, nil, nil, nil, nil)
			if got := s.SingleTenant(); got != tc.single {
				t.Errorf("SingleTenant() = %v, want %v", got, tc.single)
			}
		})
	}
}
