package service

// R-provider-1: a device-flow poll whose GitHub exchange succeeded but whose
// token the KeyStore refused must not report success — the device code is
// spent, and the client would otherwise switch to a provider with no credential.

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/testutil"
)

// grantingExchange stands in for GitHub's token endpoint: every poll grants token.
type grantingExchange struct{ token string }

func (e grantingExchange) PollToken(context.Context, string) (*ai.GitHubAuthResult, error) {
	return &ai.GitHubAuthResult{Status: "success", Token: e.token}, nil
}

// refusingSecrets is a KeyStore whose saves fail with saveErr (none when nil) and that counts its reads.
type refusingSecrets struct {
	*testutil.FakeSecretStore
	saveErr error
	gets    atomic.Int32
}

func (s *refusingSecrets) Save(scope, provider, key string) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.FakeSecretStore.Save(scope, provider, key)
}

func (s *refusingSecrets) Get(scope, provider string) (string, error) {
	s.gets.Add(1)
	return s.FakeSecretStore.Get(scope, provider)
}

// newDeviceAuthService wires a provider service whose two device flows exchange through GitHub's stand-in.
// copilot nil keeps the Copilot pre-warm (a real session-token exchange) out of a test that stores a token.
func newDeviceAuthService(secrets KeyStore, copilot *ai.CopilotAuth) *ProviderService {
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "", nil }, nil, nil, nil)
	exchange := grantingExchange{token: "gho_1"}
	return NewProviderService(ai.NewGitHubAuth(), copilot, factory, secrets, nil, nil, WithDeviceTokenExchange(exchange, exchange))
}

type deviceFlow struct {
	name string
	key  string
	poll func(*ProviderService, string) (*ai.GitHubAuthResult, error)
}

var deviceFlows = []deviceFlow{
	{"github", primaryKey(ai.GitHubModelsProviderID), func(s *ProviderService, scope string) (*ai.GitHubAuthResult, error) {
		return s.PollGitHubAuth(context.Background(), scope, "dev-1")
	}},
	{"copilot", oauthTokenKey(ai.CopilotProviderID), func(s *ProviderService, scope string) (*ai.GitHubAuthResult, error) {
		return s.PollCopilotAuth(context.Background(), scope, "dev-1")
	}},
}

// A granted exchange whose save the KeyStore refuses answers an error carrying a code the client stops on, never
// status success: 503 SECRET_STORAGE_UNAVAILABLE when the secret service is unreachable (the cause stays in the
// chain), 500 DEVICE_TOKEN_NOT_STORED for any other refusal. A save that works still answers success without the token.
func TestPollDeviceAuth_RefusedSaveIsNotSuccess(t *testing.T) {
	for _, flow := range deviceFlows {
		t.Run(flow.name, func(t *testing.T) {
			secrets := &refusingSecrets{FakeSecretStore: testutil.NewFakeSecretStore(), saveErr: storage.ErrSecretStorageUnavailable}
			res, err := flow.poll(newDeviceAuthService(secrets, ai.NewCopilotAuth()), "u1")
			if res != nil {
				t.Fatalf("secret storage unavailable: result = %+v, want none", res)
			}
			ae, ok := apperr.As(err)
			if !ok || ae.Kind.Status() != http.StatusServiceUnavailable || ae.Code != "SECRET_STORAGE_UNAVAILABLE" {
				t.Fatalf("secret storage unavailable: err = %v, want 503 SECRET_STORAGE_UNAVAILABLE", err)
			}
			if !errors.Is(err, storage.ErrSecretStorageUnavailable) {
				t.Errorf("secret storage unavailable: err = %v does not wrap storage.ErrSecretStorageUnavailable", err)
			}

			secrets = &refusingSecrets{FakeSecretStore: testutil.NewFakeSecretStore(), saveErr: errors.New("disk full")}
			res, err = flow.poll(newDeviceAuthService(secrets, ai.NewCopilotAuth()), "u1")
			if res != nil {
				t.Fatalf("other save error: result = %+v, want none", res)
			}
			ae, ok = apperr.As(err)
			if !ok || ae.Kind.Status() != http.StatusInternalServerError || ae.Code != "DEVICE_TOKEN_NOT_STORED" {
				t.Fatalf("other save error: err = %v, want 500 DEVICE_TOKEN_NOT_STORED", err)
			}

			secrets = &refusingSecrets{FakeSecretStore: testutil.NewFakeSecretStore()}
			res, err = flow.poll(newDeviceAuthService(secrets, nil), "u1")
			if err != nil || res.Status != "success" || res.Token != "" {
				t.Fatalf("stored: result = %+v, err = %v, want status success without the token", res, err)
			}
			if stored, _ := secrets.Get("u1", flow.key); stored != "gho_1" {
				t.Errorf("stored: %s = %q, want the granted token", flow.key, stored)
			}
		})
	}
}

// A refused save skips the Copilot session-token pre-warm: nothing reads the store afterwards (the pre-warm
// re-read it, found no token and returned — wasted work on a failure the poll now reports).
func TestPollCopilotAuth_RefusedSaveSkipsPreWarm(t *testing.T) {
	secrets := &refusingSecrets{FakeSecretStore: testutil.NewFakeSecretStore(), saveErr: errors.New("disk full")}
	if _, err := newDeviceAuthService(secrets, ai.NewCopilotAuth()).PollCopilotAuth(context.Background(), "u1", "dev-1"); err == nil {
		t.Fatal("a refused save answered no error")
	}
	// The pre-warm runs detached; give it longer than it needs to reach the store.
	time.Sleep(50 * time.Millisecond)
	if n := secrets.gets.Load(); n != 0 {
		t.Errorf("the store was read %d times after a refused save: the pre-warm ran", n)
	}
}
