package ai

import (
	"errors"
	"strings"
	"testing"
)

func TestStorageKey_GithubModels(t *testing.T) {
	if got := storageKey("github-models"); got != "github-models-token" {
		t.Errorf("storageKey(%q) = %q, want %q", "github-models", got, "github-models-token")
	}
}

func TestStorageKey_OtherProviders(t *testing.T) {
	for _, id := range []string{"claude", "openai", "gemini", "xai", "glm"} {
		if got := storageKey(id); got != id {
			t.Errorf("storageKey(%q) = %q, want same value", id, got)
		}
	}
}

func TestProviderFactory_For_Demo(t *testing.T) {
	f := NewProviderFactory(func(_, _ string) (string, error) { return "", nil }, nil, nil, nil)
	p, err := f.For("", "demo")
	if err != nil {
		t.Fatalf("For(%q): %v", "demo", err)
	}
	if p.ID() != "demo" {
		t.Errorf("expected demo provider, got %q", p.ID())
	}
}

func TestProviderFactory_For_Unknown(t *testing.T) {
	f := NewProviderFactory(func(_, _ string) (string, error) { return "", nil }, nil, nil, nil)
	_, err := f.For("", "nonexistent-provider")
	if err == nil {
		t.Fatal("expected error for unknown provider ID, got nil")
	}
}

func TestProviderFactory_For_KeyLookupError(t *testing.T) {
	keyErr := errors.New("keyring unavailable")
	f := NewProviderFactory(func(_, _ string) (string, error) { return "", keyErr }, nil, nil, nil)

	_, err := f.For("", "claude")
	if err == nil {
		t.Fatal("expected error when key lookup fails")
	}
	if !errors.Is(err, keyErr) {
		t.Errorf("error chain should contain the original key error; got: %v", err)
	}
}

func TestProviderFactory_For_AllKnownProviders(t *testing.T) {
	f := NewProviderFactory(func(_, _ string) (string, error) { return "test-key", nil }, nil, nil, nil)
	knownProviders := []string{"claude", "openai", "gemini", "xai", "glm", "github-models"}

	for _, id := range knownProviders {
		p, err := f.For("", id)
		if err != nil {
			t.Errorf("For(%q): unexpected error: %v", id, err)
			continue
		}
		if p == nil {
			t.Errorf("For(%q): returned nil provider", id)
		}
	}
}

func TestProviderFactory_For_Copilot_OAuth(t *testing.T) {
	keys := map[string]string{
		"copilot-oauth-token": "gh-oauth-123",
	}
	f := NewProviderFactory(func(_, k string) (string, error) { return keys[k], nil }, NewCopilotAuth(), nil, nil)

	p, err := f.For("", "copilot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID() != "copilot" {
		t.Errorf("ID = %q, want copilot", p.ID())
	}
}

func TestProviderFactory_For_Copilot_PATFallback(t *testing.T) {
	keys := map[string]string{
		"copilot": "ghp_manual_pat",
	}
	f := NewProviderFactory(func(_, k string) (string, error) { return keys[k], nil }, NewCopilotAuth(), nil, nil)

	p, err := f.For("", "copilot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID() != "copilot" {
		t.Errorf("ID = %q, want copilot", p.ID())
	}
}

func TestProviderFactory_For_Copilot_NotConfigured(t *testing.T) {
	f := NewProviderFactory(func(_, _ string) (string, error) { return "", nil }, NewCopilotAuth(), nil, nil)

	_, err := f.For("", "copilot")
	if err == nil {
		t.Fatal("expected error for unconfigured copilot")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got: %v", err)
	}
}

// TestProviderFactory_For_GithubModels_PerUserScope proves the github-models
// OAuth token is resolved under the caller's scope (not the global scope), so
// each user's connected provider is isolated.
func TestProviderFactory_For_GithubModels_PerUserScope(t *testing.T) {
	// Keys keyed by (scope, provider) — only user "u1" has the token.
	keys := map[string]string{
		"u1|github-models-token": "gho_u1_token",
	}
	getKey := func(scope, provider string) (string, error) {
		return keys[scope+"|"+provider], nil
	}
	f := NewProviderFactory(getKey, nil, nil, nil)

	// u1 is configured.
	if _, err := f.For("u1", "github-models"); err != nil {
		t.Errorf("For(u1): expected configured provider, got error: %v", err)
	}
	// u2 has no token and must report not-configured — NOT find u1's token.
	_, err := f.For("u2", "github-models")
	if err == nil {
		t.Fatal("For(u2): expected not-configured error, got nil (u1's token leaked across users)")
	}
	if !errors.Is(err, ErrKeyNotConfigured) {
		t.Errorf("For(u2): expected ErrKeyNotConfigured, got: %v", err)
	}
	// Global scope "" must not see u1's per-user token either.
	if _, err := f.For("", "github-models"); err == nil {
		t.Fatal("For(''): expected not-configured, got nil (per-user token leaked to global scope)")
	}
}

// TestProviderFactory_For_Copilot_PerUserScope proves the copilot OAuth token is
// resolved under the caller's scope (not the global scope), so one user's OAuth
// connection does not leak to others.
func TestProviderFactory_For_Copilot_PerUserScope(t *testing.T) {
	keys := map[string]string{
		"u1|copilot-oauth-token": "gho_u1_copilot",
	}
	getKey := func(scope, provider string) (string, error) {
		return keys[scope+"|"+provider], nil
	}
	f := NewProviderFactory(getKey, NewCopilotAuth(), nil, nil)

	// u1's OAuth token resolves.
	if _, err := f.For("u1", "copilot"); err != nil {
		t.Errorf("For(u1): expected configured provider, got error: %v", err)
	}
	// u2 must not find u1's token.
	if _, err := f.For("u2", "copilot"); err == nil {
		t.Fatal("For(u2): expected not-configured, got nil (u1's OAuth token leaked across users)")
	}
	// Global scope "" must not see u1's per-user token.
	if _, err := f.For("", "copilot"); err == nil {
		t.Fatal("For(''): expected not-configured, got nil (per-user OAuth token leaked to global scope)")
	}
}

// TestProviderFactory_For_Copilot_PAT_PerUserScope confirms the manual PAT path
// still works per-user after the OAuth scope change (dual-auth fallback).
func TestProviderFactory_For_Copilot_PAT_PerUserScope(t *testing.T) {
	keys := map[string]string{
		"u1|copilot": "ghp_u1_pat",
	}
	getKey := func(scope, provider string) (string, error) {
		return keys[scope+"|"+provider], nil
	}
	f := NewProviderFactory(getKey, NewCopilotAuth(), nil, nil)

	if _, err := f.For("u1", "copilot"); err != nil {
		t.Errorf("For(u1): expected PAT provider, got error: %v", err)
	}
	if _, err := f.For("u2", "copilot"); err == nil {
		t.Fatal("For(u2): expected not-configured, got nil (u1's PAT leaked across users)")
	}
}
