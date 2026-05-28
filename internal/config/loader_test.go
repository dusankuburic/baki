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

	data := `{"Mode":"cloud","Server":{"Host":"0.0.0.0","Port":8080},"Storage":{"Backend":"local"},"Auth":{"Enabled":false}}`
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
