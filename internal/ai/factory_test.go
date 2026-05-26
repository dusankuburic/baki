package ai

import (
	"errors"
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
	f := NewProviderFactory(func(string) (string, error) { return "", nil })
	p, err := f.For("demo")
	if err != nil {
		t.Fatalf("For(%q): %v", "demo", err)
	}
	if p.ID() != "demo" {
		t.Errorf("expected demo provider, got %q", p.ID())
	}
}

func TestProviderFactory_For_Unknown(t *testing.T) {
	f := NewProviderFactory(func(string) (string, error) { return "", nil })
	_, err := f.For("nonexistent-provider")
	if err == nil {
		t.Fatal("expected error for unknown provider ID, got nil")
	}
}

func TestProviderFactory_For_KeyLookupError(t *testing.T) {
	keyErr := errors.New("keyring unavailable")
	f := NewProviderFactory(func(string) (string, error) { return "", keyErr })

	_, err := f.For("claude")
	if err == nil {
		t.Fatal("expected error when key lookup fails")
	}
	if !errors.Is(err, keyErr) {
		t.Errorf("error chain should contain the original key error; got: %v", err)
	}
}

func TestProviderFactory_For_AllKnownProviders(t *testing.T) {
	f := NewProviderFactory(func(string) (string, error) { return "test-key", nil })
	knownProviders := []string{"claude", "openai", "gemini", "xai", "glm", "github-models"}

	for _, id := range knownProviders {
		p, err := f.For(id)
		if err != nil {
			t.Errorf("For(%q): unexpected error: %v", id, err)
			continue
		}
		if p == nil {
			t.Errorf("For(%q): returned nil provider", id)
		}
	}
}
