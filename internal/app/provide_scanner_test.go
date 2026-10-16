package app

import (
	"testing"

	"github.com/redis/go-redis/v9"

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
			s := provideScanner(cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			if got := s.SingleTenant(); got != tc.single {
				t.Errorf("SingleTenant() = %v, want %v", got, tc.single)
			}
		})
	}
}

// TestProvideScanner_SharesItsScansOnlyWithSeveralReplicas: the scan records go to Redis only where a second replica
// can sweep (PAD_MAX_REPLICAS > 1); a single replica keeps them in memory even with PAD_REDIS_URL set, so a Redis
// outage costs it nothing.
func TestProvideScanner_SharesItsScansOnlyWithSeveralReplicas(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // never dialled: the option only keeps it
	t.Cleanup(func() { _ = client.Close() })
	for _, tc := range []struct {
		replicas int
		client   *redis.Client
		want     bool
	}{
		{1, client, false},
		{2, client, true},
		{2, nil, false},
	} {
		cfg := serverStoreConfig(config.ModeCloud, true)
		cfg.Storage.MaxReplicas = tc.replicas
		s := provideScanner(cfg, nil, nil, nil, nil, nil, nil, nil, nil, tc.client)
		if got := s.SharesScans(); got != tc.want {
			t.Errorf("replicas %d, client %v: SharesScans() = %v, want %v", tc.replicas, tc.client != nil, got, tc.want)
		}
	}
}
