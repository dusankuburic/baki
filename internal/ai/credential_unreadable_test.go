package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A credential the key store could not read (a database blip, a document that does not decrypt) is
// ErrCredentialUnreadable, the member's and the organisation's alike, and its message is the one it always had; a
// credential that is simply absent is ErrKeyNotConfigured and never ErrCredentialUnreadable.
func TestFactory_CredentialReadFailureIsUnreadable(t *testing.T) {
	blip := errors.New("keystore: connection reset")
	f := NewProviderFactory(func(_, provider string) (string, error) {
		if provider == "openai" {
			return "", blip
		}
		return "", nil
	}, nil, nil, nil)
	f.SetOrgKeyLookup(func(_, _ string) (string, error) { return "", blip })

	_, err := f.ForEmbedding(context.Background(), "u1", "openai", "")
	if !errors.Is(err, ErrCredentialUnreadable) || !errors.Is(err, blip) || !strings.Contains(err.Error(), "get openai key: keystore: connection reset") {
		t.Errorf("member key read failure = %v; want ErrCredentialUnreadable wrapping the cause, message unchanged", err)
	}
	_, err = f.ForEmbeddingOrg(context.Background(), "u1", "org-1", "openai", "")
	if !errors.Is(err, ErrCredentialUnreadable) {
		t.Errorf("org key read failure = %v; want ErrCredentialUnreadable", err)
	}
	_, err = f.ForEmbedding(context.Background(), "u1", "glm", "")
	if !errors.Is(err, ErrKeyNotConfigured) || errors.Is(err, ErrCredentialUnreadable) {
		t.Errorf("an absent key = %v; want ErrKeyNotConfigured only", err)
	}
}
