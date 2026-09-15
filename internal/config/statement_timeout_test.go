package config

import "testing"

// PAD_DB_STATEMENT_TIMEOUT: bounded by default, overridable, and "0s" is the
// documented opt-out for a maintenance run.
func TestStorage_StatementTimeoutConfig(t *testing.T) {
	if got := Default().Storage.DBStatementTimeout; got != "" {
		t.Errorf("config default must stay empty (the backend's DefaultConfig owns the 30s), got %q", got)
	}
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_DB_STATEMENT_TIMEOUT", "2m")
	cfg, err := LoadFromEnvRaw()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.DBStatementTimeout != "2m" {
		t.Errorf("PAD_DB_STATEMENT_TIMEOUT not applied: %q", cfg.Storage.DBStatementTimeout)
	}
}
