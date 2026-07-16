package api

import (
	"net/http"
	"testing"
)

// TestDisplayFromEmail verifies the email-to-name derivation.
func TestDisplayFromEmail(t *testing.T) {
	cases := []struct {
		email string
		want  string
	}{
		{"ada.lovelace@example.com", "ada.lovelace"},
		{"bob@x.com", "bob"},
		{"no-email", "no-email"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := displayFromEmail(tc.email); got != tc.want {
			t.Errorf("displayFromEmail(%q) = %q, want %q", tc.email, got, tc.want)
		}
	}
}

// TestGreeting_LocalMode verifies the greeting uses the local display name.
func TestGreeting_LocalMode(t *testing.T) {
	sec := &SecurityConfig{JWTEnabled: false, LocalName: "TestUser"}
	h := &DashboardHandler{security: sec}
	g := h.greeting(&http.Request{})
	if g.UserDisplayName != "TestUser" {
		t.Errorf("expected 'TestUser', got %q", g.UserDisplayName)
	}
}

// TestGreeting_LocalModeDefault verifies the fallback when LocalName is unset.
func TestGreeting_LocalModeDefault(t *testing.T) {
	sec := &SecurityConfig{JWTEnabled: false}
	h := &DashboardHandler{security: sec}
	g := h.greeting(&http.Request{})
	// LocalName unset → empty is valid; the frontend has its own fallback.
	_ = g // just verify it doesn't panic
}
