package database

import "testing"

func TestSSLModeFromDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{"URL require", "postgres://u:p@host:5432/db?sslmode=require", "require"},
		{"URL verify-full", "postgres://u:p@host/db?sslmode=verify-full", "verify-full"},
		{"URL disable", "postgres://u:p@host/db?sslmode=disable", "disable"},
		{"URL none → empty", "postgres://u:p@host/db", ""},
		{"keyword require", "host=h port=5432 user=u sslmode=require dbname=db", "require"},
		{"keyword disable", "sslmode=disable host=h", "disable"},
		{"keyword none → empty", "host=h port=5432 dbname=db", ""},
		{"prefer is opportunistic", "postgres://u:p@host/db?sslmode=prefer", "prefer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sslModeFromDSN(tc.dsn); got != tc.want {
				t.Errorf("sslModeFromDSN(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
		})
	}
}

func TestSSLModeIsSecure(t *testing.T) {
	secure := []string{
		"postgres://u:p@h/db?sslmode=require",
		"postgres://u:p@h/db?sslmode=verify-ca",
		"postgres://u:p@h/db?sslmode=verify-full",
		"host=h sslmode=require",
	}
	for _, dsn := range secure {
		if !sslModeIsSecure(dsn) {
			t.Errorf("expected %q to be secure", dsn)
		}
	}
	insecure := []string{
		"postgres://u:p@h/db?sslmode=disable",
		"postgres://u:p@h/db?sslmode=allow",
		"postgres://u:p@h/db?sslmode=prefer",
		"postgres://u:p@h/db", // libpq default (prefer)
		"host=h sslmode=disable",
	}
	for _, dsn := range insecure {
		if sslModeIsSecure(dsn) {
			t.Errorf("expected %q to be insecure", dsn)
		}
	}
}

// TestNew_RejectsInsecureSSL asserts the guard fires at boot, before any
// network attempt, so it cannot pass by accidentally reaching a permissive DB.
func TestNew_RejectsInsecureSSL(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
	}{
		{"disable", "postgres://u:p@nonexistent.invalid:5432/db?sslmode=disable"},
		{"prefer", "postgres://u:p@nonexistent.invalid:5432/db?sslmode=prefer"},
		{"unset", "postgres://u:p@nonexistent.invalid:5432/db"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(t.Context(), Config{DSN: tc.dsn, RequireSSL: true})
			if err == nil {
				t.Fatalf("expected error for insecure sslmode")
			}
		})
	}
}
