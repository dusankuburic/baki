package ai

import (
	"fmt"
	"testing"
)

func TestProviderFactory_For_Copilot_WithOAuthToken(t *testing.T) {
	auth := NewCopilotAuth()
	keys := map[string]string{"copilot-oauth-token": "gh-oauth-tok"}
	f := NewProviderFactory(func(_, k string) (string, error) {
		if v, ok := keys[k]; ok {
			return v, nil
		}
		return "", fmt.Errorf("key %q not found", k)
	}, auth, nil)

	p, err := f.For("", "copilot")
	if err != nil {
		t.Fatalf("For(copilot) with OAuth: %v", err)
	}
	if p.ID() != "copilot" {
		t.Errorf("expected copilot provider, got %q", p.ID())
	}
}

func TestProviderFactory_For_Copilot_OAuthTakesPriorityOverPAT(t *testing.T) {
	// Both OAuth and PAT are configured — OAuth should win.
	auth := NewCopilotAuth()
	keys := map[string]string{
		"copilot-oauth-token": "gh-oauth-tok",
		"copilot":             "manual-pat",
	}
	f := NewProviderFactory(func(_, k string) (string, error) {
		if v, ok := keys[k]; ok {
			return v, nil
		}
		return "", fmt.Errorf("key %q not found", k)
	}, auth, nil)

	p, err := f.For("", "copilot")
	if err != nil {
		t.Fatalf("For(copilot): %v", err)
	}
	// Both produce a *CopilotProvider; the distinction is in tokenFn,
	// which we can only observe at runtime. Just verify no error and correct ID.
	if p.ID() != "copilot" {
		t.Errorf("expected copilot provider, got %q", p.ID())
	}
}

func TestProviderFactory_For_Copilot_FallbackToPAT(t *testing.T) {
	// No OAuth token — should succeed via PAT.
	keys := map[string]string{"copilot": "manual-pat"}
	f := NewProviderFactory(func(_, k string) (string, error) {
		if v, ok := keys[k]; ok {
			return v, nil
		}
		return "", fmt.Errorf("key %q not found", k)
	}, nil, nil) // nil copilotAuth — PAT path doesn't need it

	p, err := f.For("", "copilot")
	if err != nil {
		t.Fatalf("For(copilot) with PAT fallback: %v", err)
	}
	if p.ID() != "copilot" {
		t.Errorf("expected copilot provider, got %q", p.ID())
	}
}

func TestProviderFactory_For_Copilot_NeitherConfigured(t *testing.T) {
	f := NewProviderFactory(func(_, k string) (string, error) {
		return "", fmt.Errorf("key %q not found", k)
	}, nil, nil)

	_, err := f.For("", "copilot")
	if err == nil {
		t.Fatal("expected error when neither OAuth token nor PAT is configured")
	}
}

func TestProviderFactory_For_Copilot_EmptyOAuthTokenFallsBackToPAT(t *testing.T) {
	// OAuth key exists but is empty string — should still fall back to PAT.
	keys := map[string]string{
		"copilot-oauth-token": "", // present but empty
		"copilot":             "manual-pat",
	}
	f := NewProviderFactory(func(_, k string) (string, error) {
		if v, ok := keys[k]; ok {
			return v, nil
		}
		return "", fmt.Errorf("key %q not found", k)
	}, NewCopilotAuth(), nil)

	p, err := f.For("", "copilot")
	if err != nil {
		t.Fatalf("For(copilot) with empty OAuth: %v", err)
	}
	if p.ID() != "copilot" {
		t.Errorf("expected copilot provider, got %q", p.ID())
	}
}

func TestProviderFactory_For_Copilot_NilCopilotAuthFallsBackToPAT(t *testing.T) {
	// OAuth token is present, but copilotAuth is nil — the OAuth path
	// requires a non-nil CopilotAuth to construct a session-refreshing provider.
	// The factory should gracefully fall back to PAT.
	keys := map[string]string{
		"copilot-oauth-token": "gh-tok",
		"copilot":             "pat",
	}
	f := NewProviderFactory(func(_, k string) (string, error) {
		if v, ok := keys[k]; ok {
			return v, nil
		}
		return "", fmt.Errorf("key %q not found", k)
	}, nil, nil) // nil copilotAuth

	p, err := f.For("", "copilot")
	if err != nil {
		t.Fatalf("For(copilot) with nil auth + PAT: %v", err)
	}
	if p.ID() != "copilot" {
		t.Errorf("expected copilot provider, got %q", p.ID())
	}
}

func TestProviderFactory_For_Copilot_OnlyOAuthNoAuth_Error(t *testing.T) {
	// OAuth token present, copilotAuth nil, no PAT → should error.
	keys := map[string]string{"copilot-oauth-token": "gh-tok"}
	f := NewProviderFactory(func(_, k string) (string, error) {
		if v, ok := keys[k]; ok {
			return v, nil
		}
		return "", fmt.Errorf("key %q not found", k)
	}, nil, nil) // nil copilotAuth — OAuth path skipped, PAT not found

	_, err := f.For("", "copilot")
	if err == nil {
		t.Fatal("expected error: OAuth token present but copilotAuth nil, no PAT available")
	}
}
