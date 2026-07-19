package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"pad-analyzer/internal/config"
)

// fakeIdP is a minimal OIDC provider: discovery, JWKS, and a token endpoint
// that returns an RS256-signed ID token for any code.
type fakeIdP struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	clientID string
	// claims injected into the next ID token
	subject       string
	email         string
	emailVerified bool
	name          string
	nonce         string // captured from the authorize request, echoed into the token
}

func newFakeIdP(t *testing.T, clientID string) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	idp := &fakeIdP{key: key, clientID: clientID}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.server.URL,
			"authorization_endpoint":                idp.server.URL + "/authorize",
			"token_endpoint":                        idp.server.URL + "/token",
			"jwks_uri":                              idp.server.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		pub := key.Public().(*rsa.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test-key",
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		// A real IdP validates code + PKCE verifier here; the fake trusts them
		// and just mints the ID token.
		now := time.Now()
		claims := jwt.MapClaims{
			"iss":            idp.server.URL,
			"aud":            idp.clientID,
			"sub":            idp.subject,
			"email":          idp.email,
			"email_verified": idp.emailVerified,
			"name":           idp.name,
			"nonce":          idp.nonce,
			"iat":            now.Unix(),
			"exp":            now.Add(5 * time.Minute).Unix(),
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "test-key"
		idToken, err := token.SignedString(idp.key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"id_token":     idToken,
		})
	})

	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *fakeIdP) clientConfig() config.SSOConfig {
	return config.SSOConfig{
		IssuerURL:    idp.server.URL,
		ClientID:     idp.clientID,
		RedirectURL:  "http://app.example.test/api/auth/sso/callback",
		ProviderName: "fake",
	}
}

func TestClient_AuthCodeURL_CarriesPKCEAndNonce(t *testing.T) {
	idp := newFakeIdP(t, "client-1")
	c := NewClient(idp.clientConfig())

	authURL, err := c.AuthCodeURL(context.Background(), "state-1", "nonce-1", strings.Repeat("v", 43))
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()
	if q.Get("state") != "state-1" || q.Get("nonce") != "nonce-1" {
		t.Errorf("missing state/nonce in %q", authURL)
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("missing PKCE challenge in %q", authURL)
	}
	if q.Get("client_id") != "client-1" {
		t.Errorf("missing client_id in %q", authURL)
	}
}

func TestClient_Exchange_VerifiesAndExtractsIdentity(t *testing.T) {
	idp := newFakeIdP(t, "client-1")
	idp.subject = "subject-42"
	idp.email = "user@example.com"
	idp.emailVerified = true
	idp.name = "Test User"
	idp.nonce = "nonce-xyz"

	c := NewClient(idp.clientConfig())
	ident, err := c.Exchange(context.Background(), "any-code", strings.Repeat("v", 43), "nonce-xyz")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if ident.Subject != "subject-42" || ident.Email != "user@example.com" || !ident.EmailVerified || ident.DisplayName != "Test User" {
		t.Errorf("unexpected identity: %+v", ident)
	}
}

func TestClient_Exchange_RejectsNonceMismatch(t *testing.T) {
	idp := newFakeIdP(t, "client-1")
	idp.subject = "subject-42"
	idp.nonce = "nonce-from-attacker"

	c := NewClient(idp.clientConfig())
	if _, err := c.Exchange(context.Background(), "any-code", strings.Repeat("v", 43), "nonce-expected"); err == nil {
		t.Fatal("expected nonce mismatch error")
	}
}

func TestClient_Exchange_RejectsWrongAudience(t *testing.T) {
	idp := newFakeIdP(t, "some-other-client")
	idp.subject = "subject-42"
	idp.nonce = "n"

	cfg := idp.clientConfig()
	cfg.ClientID = "client-1" // we are client-1; token is minted for some-other-client
	c := NewClient(cfg)
	if _, err := c.Exchange(context.Background(), "any-code", strings.Repeat("v", 43), "n"); err == nil {
		t.Fatal("expected audience mismatch error")
	}
}

func TestClient_ProviderName_Defaults(t *testing.T) {
	c := NewClient(config.SSOConfig{})
	if c.ProviderName() != "sso" {
		t.Errorf("expected default provider name 'sso', got %q", c.ProviderName())
	}
	c = NewClient(config.SSOConfig{ProviderName: "entra"})
	if c.ProviderName() != "entra" {
		t.Errorf("expected 'entra', got %q", c.ProviderName())
	}
}

// TestClient_InvalidateProviderClearsCache guards H13: invalidateProvider
// clears the cached OIDC provider so the next call re-runs discovery
// synchronously (picks up rotated JWKS keys without a process restart).
// Exchange calls this on Verify() failure (e.g. signature mismatch from a
// rotated IdP signing key).
func TestClient_InvalidateProviderClearsCache(t *testing.T) {
	idp := newFakeIdP(t, "client-1")
	c := NewClient(idp.clientConfig())

	// Seed the cache via AuthCodeURL (cheap path through ensureProvider).
	if _, err := c.AuthCodeURL(context.Background(), "st", "n", strings.Repeat("v", 43)); err != nil {
		t.Fatalf("AuthCodeURL seed: %v", err)
	}
	c.mu.Lock()
	cachedBefore := c.provider
	c.mu.Unlock()
	if cachedBefore == nil {
		t.Fatal("expected provider to be cached after AuthCodeURL")
	}

	// Invalidate. Cache MUST be cleared.
	c.invalidateProvider()
	c.mu.Lock()
	cachedAfter := c.provider
	fetchedAfter := c.providerFetched
	c.mu.Unlock()
	if cachedAfter != nil {
		t.Errorf("expected provider cache cleared, still cached: %p", cachedAfter)
	}
	if !fetchedAfter.IsZero() {
		t.Errorf("expected providerFetched cleared, got %v", fetchedAfter)
	}

	// Next ensureProvider re-runs discovery and re-populates the cache.
	if _, err := c.AuthCodeURL(context.Background(), "st", "n", strings.Repeat("v", 43)); err != nil {
		t.Fatalf("AuthCodeURL post-invalidate: %v", err)
	}
	c.mu.Lock()
	cachedRebuilt := c.provider
	c.mu.Unlock()
	if cachedRebuilt == nil {
		t.Error("expected provider cache rebuilt after next ensureProvider")
	}
}

// TestClient_BoundedExternalCallsTimeout guards H14: if the IdP hangs, every
// external call (discovery, exchange, verify) returns within externalTimeout,
// not the caller's ctx deadline. We can't easily make the fake IdP hang without
// racing, so we point the client at a TCP listener that accepts but never
// responds — any HTTP call will block forever without the timeout wrap.
func TestClient_BoundedExternalCallsTimeout(t *testing.T) {
	// Open a TCP listener that accepts connections but never writes a response.
	// The OS will keep the connection open indefinitely.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open; do not write anything.
			_ = conn
		}
	}()

	c := NewClient(config.SSOConfig{
		IssuerURL: "http://" + ln.Addr().String(),
		ClientID:  "x",
	})

	// ensureProvider (discovery) should time out within externalTimeout+slack,
	// NOT hang forever. We bound the test's own ctx so a regression that
	// removes the timeout causes a test failure rather than a CI hang.
	ctx, cancel := context.WithTimeout(context.Background(), externalTimeout+2*time.Second)
	defer cancel()

	start := time.Now()
	_, err = c.AuthCodeURL(ctx, "st", "n", strings.Repeat("v", 43))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected discovery to fail against a hanging IdP, got nil")
	}
	if elapsed > externalTimeout+2*time.Second {
		t.Errorf("discovery took %v, expected to be bounded by %v+slack", elapsed, externalTimeout)
	}
}
