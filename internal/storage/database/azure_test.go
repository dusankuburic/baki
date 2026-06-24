package database

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// fakeCred is a stub azcore.TokenCredential that hands out a distinct token on
// each call (so the test can tell a cache hit from a refresh) with a
// configurable expiry, and can be made to fail.
type fakeCred struct {
	mu    sync.Mutex
	calls int
	exp   time.Time
	err   error
}

func (f *fakeCred) GetToken(_ context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return azcore.AccessToken{}, f.err
	}
	f.calls++
	return azcore.AccessToken{Token: fmt.Sprintf("tok-%d", f.calls), ExpiresOn: f.exp}, nil
}

func (f *fakeCred) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestConnector(cred *fakeCred) *azureMIConnector {
	return newAzureMIConnector(&azureTokenProvider{cred: cred}, nil)
}

func TestAzureMIConnector_CachesTokenUntilNearExpiry(t *testing.T) {
	cred := &fakeCred{exp: time.Now().Add(time.Hour)} // well outside the 5m refresh window
	c := newTestConnector(cred)

	tok1, err := c.currentToken(context.Background())
	if err != nil {
		t.Fatalf("currentToken: %v", err)
	}
	tok2, err := c.currentToken(context.Background())
	if err != nil {
		t.Fatalf("currentToken: %v", err)
	}
	if tok1 != tok2 {
		t.Errorf("expected cached token, got %q then %q", tok1, tok2)
	}
	if n := cred.callCount(); n != 1 {
		t.Errorf("expected 1 credential fetch (cache hit), got %d", n)
	}
}

func TestAzureMIConnector_RefreshesNearExpiry(t *testing.T) {
	cred := &fakeCred{exp: time.Now().Add(time.Minute)} // inside the 5m refresh window
	c := newTestConnector(cred)

	tok1, err := c.currentToken(context.Background())
	if err != nil {
		t.Fatalf("currentToken: %v", err)
	}
	tok2, err := c.currentToken(context.Background())
	if err != nil {
		t.Fatalf("currentToken: %v", err)
	}
	if tok1 == tok2 {
		t.Errorf("expected refreshed token near expiry, got %q twice", tok1)
	}
	if n := cred.callCount(); n != 2 {
		t.Errorf("expected 2 credential fetches (refresh), got %d", n)
	}
}

func TestAzureMIConnector_PropagatesError(t *testing.T) {
	cred := &fakeCred{err: errors.New("imds unreachable")}
	c := newTestConnector(cred)

	if _, err := c.currentToken(context.Background()); err == nil {
		t.Fatal("expected error from failing credential, got nil")
	}
}
