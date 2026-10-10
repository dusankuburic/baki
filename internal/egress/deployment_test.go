package egress

import (
	"testing"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
)

// TestForDeployment: accounts decide, not the mode. Local mode with accounts (local+db with PAD_AUTH_ENABLED) is
// multi-user, so it blocks a tenant's internal targets like cloud does; channel saves did, while the scanner allowed
// loopback there (B8). The operator's own channels (ForOperator) keep loopback outside the cloud: an upgrade must not
// refuse to boot over a localhost PAD_NOTIFY_* URL that the previous release accepted.
func TestForDeployment(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*config.Config)
		want     Policy
		operator Policy
		profile  capability.Profile
	}{
		{"desktop", func(*config.Config) {}, AllowLoopback, AllowLoopback, capability.Desktop},
		{"local+db without accounts", func(c *config.Config) { c.Storage.Backend = config.StorageDatabase }, AllowLoopback, AllowLoopback, capability.LocalDB},
		{"local+db with accounts", func(c *config.Config) {
			c.Storage.Backend = config.StorageDatabase
			c.Auth.Enabled = true
		}, BlockInternal, AllowLoopback, capability.LocalDB},
		{"cloud", func(c *config.Config) {
			c.Mode = config.ModeCloud
			c.Storage.Backend = config.StorageDatabase
			c.Auth.Enabled = true
		}, BlockInternal, BlockInternal, capability.Cloud},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default()
			c.mutate(cfg)
			caps := capability.Of(cfg)
			if caps.Profile() != c.profile {
				t.Fatalf("profile %s, want %s", caps.Profile(), c.profile)
			}
			if got := ForDeployment(caps); got != c.want {
				t.Errorf("ForDeployment = %v, want %v", got, c.want)
			}
			if got := ForOperator(caps); got != c.operator {
				t.Errorf("ForOperator = %v, want %v", got, c.operator)
			}
		})
	}
}
