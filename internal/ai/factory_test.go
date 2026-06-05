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
	f := NewProviderFactory(func(_, _ string) (string, error) { return "", nil }, nil, nil)
	p, err := f.For("", "demo")
	if err != nil {
		t.Fatalf("For(%q): %v", "demo", err)
	}
	if p.ID() != "demo" {
		t.Errorf("expected demo provider, got %q", p.ID())
	}
}

func TestProviderFactory_For_Unknown(t *testing.T) {
	f := NewProviderFactory(func(_, _ string) (string, error) { return "", nil }, nil, nil)
	_, err := f.For("", "nonexistent-provider")
	if err == nil {
		t.Fatal("expected error for unknown provider ID, got nil")
	}
}

func TestProviderFactory_For_KeyLookupError(t *testing.T) {
	keyErr := errors.New("keyring unavailable")
	f := NewProviderFactory(func(_, _ string) (string, error) { return "", keyErr }, nil, nil)

	_, err := f.For("", "claude")
	if err == nil {
		t.Fatal("expected error when key lookup fails")
	}
	if !errors.Is(err, keyErr) {
		t.Errorf("error chain should contain the original key error; got: %v", err)
	}
}

func TestProviderFactory_For_AllKnownProviders(t *testing.T) {
	f := NewProviderFactory(func(_, _ string) (string, error) { return "test-key", nil }, nil, nil)
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
	f := NewProviderFactory(func(_, k string) (string, error) { return keys[k], nil }, NewCopilotAuth(), nil)

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
	f := NewProviderFactory(func(_, k string) (string, error) { return keys[k], nil }, NewCopilotAuth(), nil)

	p, err := f.For("", "copilot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID() != "copilot" {
		t.Errorf("ID = %q, want copilot", p.ID())
	}
}

func TestProviderFactory_For_Copilot_NotConfigured(t *testing.T) {
	f := NewProviderFactory(func(_, _ string) (string, error) { return "", nil }, NewCopilotAuth(), nil)

	_, err := f.For("", "copilot")
	if err == nil {
		t.Fatal("expected error for unconfigured copilot")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got: %v", err)
	}
}
