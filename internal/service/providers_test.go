package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-core/models"
)

// fakeSecretStore is a per-(scope,provider) in-memory SecretStore for testing
// per-user key isolation.
type fakeSecretStore struct {
	keys map[string]string
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{keys: map[string]string{}}
}

func (s *fakeSecretStore) Save(scope, provider, key string) error {
	s.keys[scope+"|"+provider] = key
	return nil
}

func (s *fakeSecretStore) Get(scope, provider string) (string, error) {
	return s.keys[scope+"|"+provider], nil
}

func (s *fakeSecretStore) Has(scope, provider string) (bool, error) {
	_, ok := s.keys[scope+"|"+provider]
	return ok, nil
}

func (s *fakeSecretStore) Delete(scope, provider string) error {
	delete(s.keys, scope+"|"+provider)
	return nil
}

func newProviderServiceWithFakeSecrets(t *testing.T, secrets *fakeSecretStore) *ProviderService {
	t.Helper()
	getKey := func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}
	factory := ai.NewProviderFactory(getKey, ai.NewCopilotAuth(), nil, nil)
	return NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets)
}

// TestListProviders_GithubModels_PerUserConfigured verifies that a github-models
// OAuth token stored under a user's scope marks the provider configured for that
// user only — not for other users or the global scope.
func TestListProviders_GithubModels_PerUserConfigured(t *testing.T) {
	secrets := newFakeSecretStore()
	if err := secrets.Save("u1", "github-models-token", "gho_u1"); err != nil {
		t.Fatal(err)
	}
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	providers, err := svc.ListProviders(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ListProviders(u1): %v", err)
	}
	if gm := findProvider(providers, "github-models"); gm == nil {
		t.Fatal("github-models provider missing from list")
	} else if !gm.Configured {
		t.Error("github-models should be Configured for u1 (per-user token stored)")
	}

	// A different user must NOT see u1's token as configured.
	providers, _ = svc.ListProviders(context.Background(), "u2")
	if gm := findProvider(providers, "github-models"); gm != nil && gm.Configured {
		t.Error("github-models should NOT be Configured for u2 (token leaked across users)")
	}
}

// TestListProviders_Copilot_PerUserConfigured verifies the copilot OAuth token is
// resolved per-user.
func TestListProviders_Copilot_PerUserConfigured(t *testing.T) {
	secrets := newFakeSecretStore()
	if err := secrets.Save("u1", "copilot-oauth-token", "gho_u1"); err != nil {
		t.Fatal(err)
	}
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	providers, err := svc.ListProviders(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ListProviders(u1): %v", err)
	}
	if c := findProvider(providers, "copilot"); c == nil {
		t.Fatal("copilot provider missing from list")
	} else if !c.Configured {
		t.Error("copilot should be Configured for u1 (per-user OAuth token stored)")
	}

	providers, _ = svc.ListProviders(context.Background(), "u2")
	if c := findProvider(providers, "copilot"); c != nil && c.Configured {
		t.Error("copilot should NOT be Configured for u2 (OAuth token leaked across users)")
	}
}

func findProvider(ps []models.ProviderInfo, id string) *models.ProviderInfo {
	for i := range ps {
		if ps[i].ID == id {
			return &ps[i]
		}
	}
	return nil
}
