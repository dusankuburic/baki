package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.Mode != ModeLocal {
		t.Errorf("expected mode %q, got %q", ModeLocal, cfg.Mode)
	}
	if cfg.Auth.Enabled {
		t.Error("auth should be disabled by default")
	}
	if cfg.Storage.Backend != StorageLocal {
		t.Errorf("expected storage backend %q, got %q", StorageLocal, cfg.Storage.Backend)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("unexpected error for missing file: %v", err)
	}
	if cfg.Mode != ModeLocal {
		t.Errorf("expected default mode, got %q", cfg.Mode)
	}
}

func TestLoad_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	data := `{"Mode":"cloud","Server":{"Host":"0.0.0.0","Port":8080},"Storage":{"Backend":"database","DatabaseURL":"postgres://localhost/test"},"Auth":{"Enabled":false}}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode != ModeCloud {
		t.Errorf("expected cloud mode, got %q", cfg.Mode)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Server.Port)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("{bad json}"), 0644)

	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestValidate_AuthSecretRequired(t *testing.T) {
	cfg := Default()
	cfg.Auth.Enabled = true
	cfg.Auth.Secret = ""

	if err := Validate(cfg); err == nil {
		t.Error("expected validation error when auth enabled with no secret")
	}
}

func TestValidate_DatabaseURLRequired(t *testing.T) {
	cfg := Default()
	cfg.Storage.Backend = StorageDatabase
	cfg.Storage.DatabaseURL = ""

	if err := Validate(cfg); err == nil {
		t.Error("expected validation error when database backend with no URL")
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := Default()
	original.Server.Port = 9000
	original.Mode = ModeCloud
	original.Storage.Backend = StorageDatabase
	original.Storage.DatabaseURL = "postgres://localhost/test"

	if err := Save(original, path); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	if loaded.Server.Port != 9000 {
		t.Errorf("port mismatch: got %d", loaded.Server.Port)
	}
	if loaded.Mode != ModeCloud {
		t.Errorf("mode mismatch: got %q", loaded.Mode)
	}
}

func cloudCfg() *Config {
	cfg := Default()
	cfg.Mode = ModeCloud
	cfg.Auth.Enabled = true
	cfg.Storage.Backend = StorageDatabase
	cfg.Storage.DatabaseURL = "postgres://localhost/db"
	cfg.Auth.Secret = "a-sufficiently-long-random-secret-value-123456"
	// Most existing tests don't care about the TLS posture; declare a
	// reverse proxy so the plaintext-credentials safety check passes.
	// Tests that specifically exercise the TLS validation override this.
	cfg.Server.BehindProxy = true
	return cfg
}

func TestValidate_CloudSecretStrength(t *testing.T) {
	t.Run("strong secret passes", func(t *testing.T) {
		if err := Validate(cloudCfg()); err != nil {
			t.Fatalf("expected strong secret to pass, got %v", err)
		}
	})
	t.Run("short secret rejected", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Auth.Secret = "too-short"
		if err := Validate(cfg); err == nil {
			t.Fatal("expected short secret to be rejected in cloud mode")
		}
	})
	t.Run("known default rejected", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Auth.Secret = "change-me-in-production"
		if err := Validate(cfg); err == nil {
			t.Fatal("expected known-default secret to be rejected in cloud mode")
		}
	})
	t.Run("weak secret allowed in local mode", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Mode = ModeLocal
		cfg.Auth.Enabled = false
		cfg.Auth.Secret = ""
		if err := Validate(cfg); err != nil {
			t.Fatalf("local mode should not enforce secret strength, got %v", err)
		}
	})
}

func TestValidate_WildcardOriginRejected(t *testing.T) {
	cfg := cloudCfg()
	cfg.Server.AllowedOrigins = []string{"*"}
	if err := Validate(cfg); err == nil {
		t.Fatal("expected wildcard origin to be rejected")
	}
}

func TestValidate_TLSConfig(t *testing.T) {
	t.Run("local mode allows plaintext", func(t *testing.T) {
		cfg := Default() // ModeLocal, Auth disabled
		if err := Validate(cfg); err != nil {
			t.Errorf("local mode should not require TLS, got %v", err)
		}
	})
	t.Run("cloud + auth + no TLS + no proxy is rejected", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Server.BehindProxy = false
		cfg.Server.TLSCert = ""
		cfg.Server.TLSKey = ""
		if err := Validate(cfg); err == nil {
			t.Fatal("expected validation error for plaintext cloud auth")
		}
	})
	t.Run("cloud + auth + TLS cert/key passes", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Server.BehindProxy = false
		cfg.Server.TLSCert = "/etc/tls/cert.pem"
		cfg.Server.TLSKey = "/etc/tls/key.pem"
		if err := Validate(cfg); err != nil {
			t.Errorf("cloud + TLS should pass, got %v", err)
		}
	})
	t.Run("cloud + auth + BehindProxy passes", func(t *testing.T) {
		cfg := cloudCfg() // already sets BehindProxy=true
		if err := Validate(cfg); err != nil {
			t.Errorf("cloud + BehindProxy should pass, got %v", err)
		}
	})
	t.Run("partial TLS (cert without key) is rejected", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Server.TLSCert = "/etc/tls/cert.pem"
		cfg.Server.TLSKey = ""
		if err := Validate(cfg); err == nil {
			t.Fatal("expected error for partial TLS config")
		}
	})
}

func TestValidate_TrustedProxies(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		wantErr bool
	}{
		{"empty list ok", nil, false},
		{"single IPv4 ok", []string{"10.0.0.1"}, false},
		{"single IPv6 ok", []string{"::1"}, false},
		{"CIDR ok", []string{"10.0.0.0/8", "192.168.1.0/24"}, false},
		{"mixed IP + CIDR ok", []string{"10.0.0.1", "192.168.0.0/16"}, false},
		{"trims whitespace", []string{" 10.0.0.1 "}, false},
		{"rejects wildcard *", []string{"*"}, true},
		{"rejects 0.0.0.0/0", []string{"0.0.0.0/0"}, true},
		{"rejects ::/0", []string{"::/0"}, true},
		{"rejects hostname", []string{"example.com"}, true},
		{"rejects bogus CIDR", []string{"10.0.0.0/64"}, true},
		{"rejects garbage", []string{"not-an-ip"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cloudCfg()
			cfg.Server.TrustedProxies = tc.entries
			err := Validate(cfg)
			if tc.wantErr && err == nil {
				t.Errorf("entries=%v: expected error, got nil", tc.entries)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("entries=%v: unexpected error: %v", tc.entries, err)
			}
		})
	}
}
