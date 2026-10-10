package app

import (
	"testing"
	"time"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
)

// TestDrainDelayDefault_FollowsTheCloudProfile: PAD_SHUTDOWN_DRAIN_DELAY defaults to 3 s in capability's Cloud
// profile and to 0 in the others. config.Load derives it from the mode, because internal/config cannot import
// capability (capability is derived from a Config); this keeps the two in step.
func TestDrainDelayDefault_FollowsTheCloudProfile(t *testing.T) {
	for _, env := range []map[string]string{
		{}, // desktop: no PAD_MODE, no PAD_PORT
		{"PAD_MODE": "local"},
		{"PAD_MODE": "local", "PAD_STORAGE": "database", "PAD_DATABASE_URL": "postgres://localhost/test"},
		{"PAD_MODE": "local", "PAD_STORAGE": "database", "PAD_DATABASE_URL": "postgres://localhost/test", "PAD_AUTH_ENABLED": "true", "PAD_AUTH_SECRET": "x"},
		{"PAD_MODE": "cloud", "PAD_STORAGE": "database", "PAD_DATABASE_URL": "postgres://localhost/test", "PAD_AUTH_ENABLED": "true",
			"PAD_AUTH_SECRET": "a-long-random-cloud-signing-secret-0123456789", "PAD_BEHIND_PROXY": "true"},
	} {
		for _, k := range []string{"PAD_CONFIG", "PAD_MODE", "PAD_PORT", "PAD_STORAGE", "PAD_DATABASE_URL", "PAD_AUTH_ENABLED",
			"PAD_AUTH_SECRET", "PAD_BEHIND_PROXY", "PAD_SHUTDOWN_DRAIN_DELAY"} {
			t.Setenv(k, env[k])
		}
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("%v: Load: %v", env, err)
		}
		profile := capability.Of(cfg).Profile()
		want := time.Duration(0)
		if profile == capability.Cloud {
			want = config.DefaultCloudShutdownDrainDelay
		}
		if got := cfg.Runtime.ShutdownDrainDelay.Std(); got != want {
			t.Errorf("profile %s: drain delay default %v, want %v", profile, got, want)
		}
	}
}
