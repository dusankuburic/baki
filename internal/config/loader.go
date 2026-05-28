package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Load reads configuration from the given JSON file.
// If the file does not exist it returns Default() without an error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	cfg := Default()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := Validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Save writes the configuration to the given JSON file.
func Save(cfg *Config, path string) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// LoadFromEnv builds a Config from PAD_* environment variables.
// Recognised variables:
//
//	PAD_MODE             "local" or "cloud"      (default: "local")
//	PAD_HOST             bind host               (default: "localhost")
//	PAD_PORT             bind port               (default: 0 = ephemeral)
//	PAD_ALLOWED_ORIGINS  comma-separated origins for CORS/WebSocket
//	PAD_TRUSTED_PROXIES  comma-separated IPs of trusted reverse proxies
//	PAD_DATA_DIR         local storage root
//	PAD_STORAGE          "local" or "database"
//	PAD_DATABASE_URL     postgres DSN            (required when PAD_STORAGE=database)
//	PAD_AUTH_ENABLED     "true" / "false"        (default: "false")
//	PAD_AUTH_SECRET      JWT signing key
func LoadFromEnv() (*Config, error) {
	cfg := Default()

	if v := os.Getenv("PAD_MODE"); v != "" {
		cfg.Mode = DeploymentMode(v)
	}
	if v := os.Getenv("PAD_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("PAD_ALLOWED_ORIGINS"); v != "" {
		for _, o := range strings.Split(v, ",") {
			if trimmed := strings.TrimSpace(o); trimmed != "" {
				cfg.Server.AllowedOrigins = append(cfg.Server.AllowedOrigins, trimmed)
			}
		}
	}
	if v := os.Getenv("PAD_TRUSTED_PROXIES"); v != "" {
		for _, p := range strings.Split(v, ",") {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				cfg.Server.TrustedProxies = append(cfg.Server.TrustedProxies, trimmed)
			}
		}
	}
	if v := os.Getenv("PAD_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: PAD_PORT is not a valid integer: %w", err)
		}
		cfg.Server.Port = p
	}
	if v := os.Getenv("PAD_DATA_DIR"); v != "" {
		cfg.Storage.DataDir = v
	}
	if v := os.Getenv("PAD_STORAGE"); v != "" {
		cfg.Storage.Backend = StorageBackend(v)
	}
	if v := os.Getenv("PAD_DATABASE_URL"); v != "" {
		cfg.Storage.DatabaseURL = v
	}
	if v := os.Getenv("PAD_AUTH_ENABLED"); v != "" {
		cfg.Auth.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("PAD_AUTH_SECRET"); v != "" {
		cfg.Auth.Secret = v
	}

	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks that the configuration is internally consistent.
func Validate(cfg *Config) error {
	if cfg.Mode != ModeLocal && cfg.Mode != ModeCloud {
		return fmt.Errorf("config: unknown deployment mode %q", cfg.Mode)
	}
	if cfg.Auth.Enabled && cfg.Auth.Secret == "" {
		return errors.New("config: auth.secret is required when auth.enabled is true")
	}
	if cfg.Storage.Backend == StorageDatabase && cfg.Storage.DatabaseURL == "" {
		return errors.New("config: storage.database_url is required when storage.backend is database")
	}
	return nil
}
