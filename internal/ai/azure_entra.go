package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"golang.org/x/sync/singleflight"

	"pad-core/logger"
)

// azureFoundryScope is the Entra ID token audience for every Foundry data
// plane (model inference, agents, project APIs).
const azureFoundryScope = "https://ai.azure.com/.default"

// entraTokenSource caches one service principal's access token and refreshes
// it shortly before expiry. It mirrors the Postgres managed-identity
// connector (storage/database/azure.go): the cache is read under a mutex, the
// network fetch runs outside it under singleflight, so a refresh never
// serialises concurrent chat turns and a burst of turns at a token boundary
// costs one token request, not one per turn.
type entraTokenSource struct {
	cred azcore.TokenCredential

	mu      sync.Mutex
	token   string
	expires time.Time
	sf      singleflight.Group
}

func newEntraTokenSource(cred azcore.TokenCredential) *entraTokenSource {
	return &entraTokenSource{cred: cred}
}

// Token returns a valid access token, fetching or refreshing when the cached
// one is missing or within 5 minutes of expiry.
func (s *entraTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	tok, exp := s.token, s.expires
	s.mu.Unlock()
	if tok != "" && time.Until(exp) >= 5*time.Minute {
		return tok, nil
	}
	v, err, _ := s.sf.Do("token", func() (any, error) {
		at, err := s.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{azureFoundryScope}})
		if err != nil {
			return "", fmt.Errorf("azure ai foundry: entra id token: %w", err)
		}
		s.mu.Lock()
		s.token = at.Token
		s.expires = at.ExpiresOn
		s.mu.Unlock()
		logger.Debug("azure ai foundry: refreshed entra id token", "expires", at.ExpiresOn)
		return at.Token, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// Invalidate drops the cached token if it is still the one that just failed
// (401 from the service), so the next Token call fetches a fresh one. A newer
// token another goroutine already refreshed is left alone.
func (s *entraTokenSource) Invalidate(failed string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == failed {
		s.token = ""
		s.expires = time.Time{}
	}
}

// entraSources keeps one token source per service principal, process-wide.
// The factory builds a fresh provider per request; without this registry every
// chat turn would create a new credential and pay a client-credentials round
// trip to login.microsoftonline.com. Keyed by a hash of the credential so a
// rotated secret gets its own source (and never reuses a token minted for the
// old one).
var entraSources = struct {
	mu sync.Mutex
	m  map[string]*entraTokenSource
}{m: map[string]*entraTokenSource{}}

// entraSourcesMax bounds the registry; a deployment realistically holds a
// handful of service principals.
const entraSourcesMax = 256

func entraSourceFor(tenantID, clientID, clientSecret string) (*entraTokenSource, error) {
	h := sha256.Sum256([]byte(tenantID + "\x00" + clientID + "\x00" + clientSecret))
	key := hex.EncodeToString(h[:])

	entraSources.mu.Lock()
	defer entraSources.mu.Unlock()
	if s, ok := entraSources.m[key]; ok {
		return s, nil
	}
	cred, err := azidentity.NewClientSecretCredential(tenantID, clientID, clientSecret, nil)
	if err != nil {
		return nil, fmt.Errorf("azure ai foundry: entra id credential: %w", err)
	}
	if len(entraSources.m) >= entraSourcesMax {
		// Rare (hundreds of distinct principals): start over rather than
		// grow without bound. Dropped sources just re-fetch on next use.
		entraSources.m = map[string]*entraTokenSource{}
	}
	s := newEntraTokenSource(cred)
	entraSources.m[key] = s
	return s, nil
}

// resetEntraSources clears the registry (tests).
func resetEntraSources() {
	entraSources.mu.Lock()
	defer entraSources.mu.Unlock()
	entraSources.m = map[string]*entraTokenSource{}
}
