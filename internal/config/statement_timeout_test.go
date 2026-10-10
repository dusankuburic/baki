package config

import (
	"testing"
	"time"
)

// PAD_DB_STATEMENT_TIMEOUT: bounded by default (the backend's 30s, held here so
// "0s" and unset differ), overridable, and "0s" is the documented opt-out for a
// maintenance run.
func TestStorage_StatementTimeoutConfig(t *testing.T) {
	if got := Default().Storage.DBStatementTimeout.Std(); got != 30*time.Second {
		t.Errorf("config default = %v, want the backend's 30s", got)
	}
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_DB_STATEMENT_TIMEOUT", "2m")
	cfg, err := LoadFromEnvRaw()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.DBStatementTimeout.Std() != 2*time.Minute {
		t.Errorf("PAD_DB_STATEMENT_TIMEOUT not applied: %v", cfg.Storage.DBStatementTimeout)
	}
	t.Setenv("PAD_DB_STATEMENT_TIMEOUT", "0s")
	cfg, err = LoadFromEnvRaw()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.DBStatementTimeout != 0 {
		t.Errorf(`PAD_DB_STATEMENT_TIMEOUT=0s = %v, want 0 (off)`, cfg.Storage.DBStatementTimeout)
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("0s must stay a valid opt-out: %v", err)
	}
}
