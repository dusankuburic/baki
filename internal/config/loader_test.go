package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestLoadRaw_MissingFile(t *testing.T) {
	cfg, err := LoadRaw(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("unexpected error for missing file: %v", err)
	}
	if cfg.Mode != ModeLocal {
		t.Errorf("expected default mode, got %q", cfg.Mode)
	}
}

func TestLoadRaw_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	data := `{"Mode":"cloud","Server":{"Host":"0.0.0.0","Port":8080,"BehindProxy":true},"Storage":{"Backend":"database","DatabaseURL":"postgres://localhost/test"},"Auth":{"Enabled":true,"Secret":"a-long-random-cloud-signing-secret"}}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRaw(path)
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

func TestLoadRaw_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("{bad json}"), 0644)

	_, err := LoadRaw(path)
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

// TestValidate_CloudRequiresAuth is the regression test for the misconfig
// footgun: cloud (multi-tenant) mode with auth disabled silently bypasses JWT,
// RLS, and role checks. Validation must reject it so an operator who forgets
// PAD_AUTH_ENABLED=true can't deploy an unauthenticated multi-tenant system.
func TestValidate_CloudRequiresAuth(t *testing.T) {
	cfg := cloudCfg()
	cfg.Auth.Enabled = false
	if err := Validate(cfg); err == nil {
		t.Fatal("expected validation error for cloud mode with auth disabled")
	}
}

// TestLoadFromEnvRaw_NormalizesStorageCase confirms PAD_STORAGE is case-folded
// so "LOCAL"/"Database" typos don't fall through to an unknown backend (which
// in cloud mode would silently run with no storage backend).
func TestLoadFromEnvRaw_NormalizesStorageCase(t *testing.T) {
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_STORAGE", "LOCAL") // capitalised — would be unknown without normalization
	cfg, err := LoadFromEnvRaw()
	if err != nil {
		t.Fatalf("capitalised PAD_STORAGE should be accepted (case-folded): %v", err)
	}
	if cfg.Storage.Backend != StorageLocal {
		t.Errorf("PAD_STORAGE=LOCAL normalized to %q, want %q", cfg.Storage.Backend, StorageLocal)
	}

	t.Setenv("PAD_STORAGE", "garbage")
	if _, err := LoadFromEnvRaw(); err == nil {
		t.Fatal("expected error for unknown PAD_STORAGE value")
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

// The dead validating wrappers (Load/Save/LoadFromEnv) were removed — main
// composes LoadRaw/LoadFromEnvRaw + Validate itself, and the round-trip save
// path never shipped. Their behavior coverage lives in the LoadRaw tests above.

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

// TestValidate_PprofRequiresMetricsToken locks in the fail-closed half of the
// pprof exposure fix. The route-registration half is covered by
// api.TestPprof_NotRegisteredByDefault; this covers the case where an operator
// deliberately turns profiling on and would otherwise get no gate at all,
// because MetricsGuard's IP allowlist is vacuous behind a reverse proxy.
func TestValidate_PprofRequiresMetricsToken(t *testing.T) {
	t.Run("disabled needs no token", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Server.PprofEnabled = false
		cfg.Server.MetricsToken = ""
		if err := Validate(cfg); err != nil {
			t.Errorf("pprof off should not require a token, got %v", err)
		}
	})
	t.Run("enabled without a token is rejected", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Server.PprofEnabled = true
		cfg.Server.MetricsToken = ""
		err := Validate(cfg)
		if err == nil {
			t.Fatal("expected validation error: profiling on with no PAD_METRICS_TOKEN")
		}
		// The operator needs to know WHICH variable to set.
		if !strings.Contains(err.Error(), "PAD_METRICS_TOKEN") {
			t.Errorf("error must name the variable to set, got %q", err)
		}
	})
	t.Run("whitespace-only token is not a token", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Server.PprofEnabled = true
		cfg.Server.MetricsToken = "   "
		if err := Validate(cfg); err == nil {
			t.Fatal("expected validation error for a blank token")
		}
	})
	t.Run("enabled with a token passes", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Server.PprofEnabled = true
		cfg.Server.MetricsToken = "a-real-shared-secret"
		if err := Validate(cfg); err != nil {
			t.Errorf("pprof on with a token should pass, got %v", err)
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

// TestApplyEnvVars_BadNumericRejected confirms the silent-swallow pattern was
// fixed: a typo in a numeric env var now returns an error at load time instead
// of silently keeping the default (which could leave a looser rate limit than
// the operator intended).
func TestApplyEnvVars_BadNumericRejected(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
	}{
		{"bad rate-limit float", "PAD_RATE_LIMIT_AUTH_RPS", "1O"}, // letter O
		{"bad CB integer", "PAD_CB_FAILURES", "abc"},
		{"bad retry integer", "PAD_RETRY_MAX_ATTEMPTS", "x"},
		{"bad audit retention", "PAD_AUDIT_RETENTION_DAYS", "not-a-number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			cfg := Default()
			if err := applyEnvVars(cfg); err == nil {
				t.Errorf("%s=%q: expected error, got nil", tc.env, tc.val)
			}
		})
	}
}

// TestApplyEnvVars_NegativeAuditRetentionRejected confirms a negative
// AuditRetentionDays is rejected (previously silently dropped, leaving the
// default — wrong retention schedule with no feedback).
func TestApplyEnvVars_NegativeAuditRetentionRejected(t *testing.T) {
	t.Setenv("PAD_AUDIT_RETENTION_DAYS", "-5")
	cfg := Default()
	if err := applyEnvVars(cfg); err == nil {
		t.Fatal("expected error for negative PAD_AUDIT_RETENTION_DAYS")
	}
}

// TestApplyEnvVars_BadDurationRejected confirms duration env vars are validated
// at load time (previously stored as raw strings with no parse, failing late
// and inconsistently at runtime).
func TestApplyEnvVars_BadDurationRejected(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"bad scan interval", "PAD_SCAN_INTERVAL"},
		{"bad CB open duration", "PAD_CB_OPEN_DURATION"},
		{"bad retry base delay", "PAD_RETRY_BASE_DELAY"},
		{"bad request timeout", "PAD_REQUEST_TIMEOUT"},
		{"bad DB conn lifetime", "PAD_DB_CONN_MAX_LIFETIME"},
		{"bad PP ingest interval", "PAD_PP_INGEST_INTERVAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, "1hour") // should be "1h"
			cfg := Default()
			if err := applyEnvVars(cfg); err == nil {
				t.Errorf("%s=1hour: expected error, got nil", tc.env)
			}
		})
	}
}

// TestApplyEnvVars_GoodNumericAndDurationAccepted confirms valid values pass
// the new validation (regression guard against over-strict parsing).
func TestApplyEnvVars_GoodNumericAndDurationAccepted(t *testing.T) {
	t.Setenv("PAD_RATE_LIMIT_AUTH_RPS", "0.5")
	t.Setenv("PAD_CB_FAILURES", "7")
	t.Setenv("PAD_AUDIT_RETENTION_DAYS", "90")
	t.Setenv("PAD_SCAN_INTERVAL", "30m")
	t.Setenv("PAD_REQUEST_TIMEOUT", "45s")
	cfg := Default()
	if err := applyEnvVars(cfg); err != nil {
		t.Fatalf("expected valid values to pass, got: %v", err)
	}
	if cfg.Runtime.RateLimitAuthRPS != 0.5 {
		t.Errorf("RateLimitAuthRPS = %v, want 0.5", cfg.Runtime.RateLimitAuthRPS)
	}
	if cfg.Runtime.CircuitBreakerFailures != 7 {
		t.Errorf("CircuitBreakerFailures = %v, want 7", cfg.Runtime.CircuitBreakerFailures)
	}
	if cfg.Governance.AuditRetentionDays != 90 {
		t.Errorf("AuditRetentionDays = %v, want 90", cfg.Governance.AuditRetentionDays)
	}
}

// TestApplyEnvVars_EmbeddingDim confirms PAD_EMBEDDING_DIM is wired into the
// storage config (it gates which knowledge chunks are pgvector-searchable).
func TestApplyEnvVars_EmbeddingDim(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		t.Setenv("PAD_EMBEDDING_DIM", "3072")
		cfg := Default()
		if err := applyEnvVars(cfg); err != nil {
			t.Fatalf("applyEnvVars: %v", err)
		}
		if cfg.Storage.EmbeddingDim != 3072 {
			t.Errorf("EmbeddingDim = %d, want 3072", cfg.Storage.EmbeddingDim)
		}
	})
	t.Run("unset leaves zero (storage layer applies the 1536 default)", func(t *testing.T) {
		cfg := Default()
		if err := applyEnvVars(cfg); err != nil {
			t.Fatalf("applyEnvVars: %v", err)
		}
		if cfg.Storage.EmbeddingDim != 0 {
			t.Errorf("EmbeddingDim = %d, want 0 (default applied downstream)", cfg.Storage.EmbeddingDim)
		}
	})
}

// TestValidate_CloudEncryptionKeyStrength confirms the dedicated encryption key
// gets the same strength gate as the JWT secret in cloud mode (prevents
// PAD_ENCRYPTION_KEY="x" from protecting stored provider credentials).
func TestValidate_CloudEncryptionKeyStrength(t *testing.T) {
	t.Run("strong encryption key passes", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Auth.EncryptionKey = "a-sufficiently-long-random-enc-key-123456"
		if err := Validate(cfg); err != nil {
			t.Fatalf("expected strong encryption key to pass, got %v", err)
		}
	})
	t.Run("short encryption key rejected", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Auth.EncryptionKey = "x"
		if err := Validate(cfg); err == nil {
			t.Fatal("expected short encryption key to be rejected in cloud mode")
		}
	})
	t.Run("empty encryption key allowed (falls back to auth secret)", func(t *testing.T) {
		cfg := cloudCfg()
		cfg.Auth.EncryptionKey = ""
		if err := Validate(cfg); err != nil {
			t.Fatalf("expected empty encryption key to pass (fallback), got %v", err)
		}
	})
}

// TestIsWeakSecret_BlocklistAndEntropy locks the expanded weak-secret gate
// (L6): the longer documented placeholders that pass the 32-char floor are
// rejected, and long-but-pathological low-entropy secrets (repeated chars /
// tiles) are caught even though they aren't on the literal blocklist.
func TestIsWeakSecret_BlocklistAndEntropy(t *testing.T) {
	// Long placeholders that previously slipped past the length floor alone.
	for _, s := range []string{
		"please-change-this-secret-in-production",
		"your-super-secret-jwt-key-change-me",
		"your-256-bit-secret-here-replace-it",
		strings.Repeat("x", 36),
	} {
		if !isWeakSecret(s) {
			t.Errorf("isWeakSecret(%q) = false, want true (known placeholder)", s)
		}
	}

	// Low-entropy: long enough but obviously non-random.
	weak := []string{
		strings.Repeat("a", 64),             // all identical
		strings.Repeat("0", 40),             // all identical digits
		"ab" + strings.Repeat("ab", 20),     // 2-byte tile (42 chars)
		"abc" + strings.Repeat("abc", 14),   // 3-byte tile (45 chars)
		"abcd" + strings.Repeat("abcd", 10), // 4-byte tile (44 chars)
	}
	for _, s := range weak {
		if !isLowEntropy(s) {
			t.Errorf("isLowEntropy(%q) = false, want true", s)
		}
		if !isWeakSecret(s) {
			t.Errorf("isWeakSecret(%q) = false, want true (low entropy)", s)
		}
	}

	// A genuinely long, varied secret must NOT be flagged.
	good := "x9K2mP7qR4vN8wL3jH6tB1yY5cZ0dF4aEsUgT"
	if isWeakSecret(good) {
		t.Errorf("isWeakSecret(good secret) = true, want false (false positive)")
	}
}
