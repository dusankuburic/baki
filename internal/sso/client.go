// Package sso implements the OIDC relying-party side of account-level single
// sign-on: discovery, the authorization-code redirect (with PKCE), and code →
// verified-identity exchange. It is IdP-agnostic — anything that serves a
// spec-compliant discovery document works (Microsoft Entra ID, Google, Okta,
// Keycloak, ...).
package sso

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"pad-analyzer/internal/config"
)

// Identity is the verified result of an OIDC login, reduced to the claims the
// app needs for find-or-create.
type Identity struct {
	// Subject is the IdP's stable user identifier (the `sub` claim).
	Subject string
	// Email as asserted by the IdP. May be empty.
	Email string
	// EmailVerified reports whether the IdP asserts it has verified Email.
	// Auto-linking to an existing local account by email requires this.
	EmailVerified bool
	// DisplayName from the `name` claim, when present.
	DisplayName string
}

// providerTTL bounds how long a cached OIDC provider is trusted without a
// background refresh. IdPs (Entra ID, Google, Okta) routinely rotate signing
// keys; a cached Verifier built from stale JWKS rejects every login until the
// process restarts. 15 min is short enough to track a key rotation but long
// enough to amortize discovery across many logins.
const providerTTL = 15 * time.Minute

// externalTimeout bounds each outbound call to the IdP (discovery, code
// exchange, ID token verification). The caller's ctx may have a much longer
// deadline (e.g. an fx root ctx); without this wrap a hung IdP endpoint pins
// handler goroutines indefinitely.
const externalTimeout = 15 * time.Second

// Client wraps the OIDC provider for one configured IdP. Discovery is lazy:
// the IdP is first contacted on the first login attempt, not at startup, so a
// temporarily unreachable IdP doesn't prevent the app from booting.
type Client struct {
	cfg config.SSOConfig

	mu              sync.Mutex
	provider        *oidc.Provider
	providerFetched time.Time // when c.provider was last refreshed
}

func NewClient(cfg config.SSOConfig) *Client {
	return &Client{cfg: cfg}
}

// ProviderName returns the display label / identity_links provider key.
func (c *Client) ProviderName() string {
	if c.cfg.ProviderName != "" {
		return c.cfg.ProviderName
	}
	return "sso"
}

// ensureProvider performs OIDC discovery (or returns a cached provider).
//
// The cache has a bounded TTL (providerTTL): once it elapses, the next call
// triggers a refresh. Refresh failures fall back to the stale provider if one
// exists (better than hard-failing every login during a transient IdP blip),
// or surface the error if there is no prior provider to fall back to.
//
// On id_token verification failure the caller should call invalidateProvider
// to force a synchronous refresh on the next attempt (handles JWKS key
// rotation between TTL ticks).
//
// The mutex is NOT held during the HTTP discovery call, so concurrent SSO
// logins don't serialize behind one stuck network round-trip.
func (c *Client) ensureProvider(ctx context.Context) (*oidc.Provider, error) {
	c.mu.Lock()
	if c.provider != nil && time.Since(c.providerFetched) < providerTTL {
		p := c.provider
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()

	// Bound discovery with its own deadline so a hung IdP doesn't pin the
	// caller's goroutine past externalTimeout.
	discCtx, cancel := context.WithTimeout(ctx, externalTimeout)
	defer cancel()
	p, err := oidc.NewProvider(discCtx, c.cfg.IssuerURL)
	if err != nil {
		// If we have a stale provider younger than ~1h, prefer it over failing
		// the login entirely — a brief IdP blip shouldn't lock users out.
		c.mu.Lock()
		if c.provider != nil && time.Since(c.providerFetched) < time.Hour {
			p = c.provider
			c.mu.Unlock()
			return p, nil
		}
		c.mu.Unlock()
		return nil, fmt.Errorf("sso: discovery for %s failed: %w", c.cfg.IssuerURL, err)
	}

	c.mu.Lock()
	if c.provider == nil || time.Since(c.providerFetched) >= providerTTL {
		c.provider = p
		c.providerFetched = time.Now()
	} else {
		p = c.provider // another caller won the race; use theirs
	}
	c.mu.Unlock()
	return p, nil
}

// invalidateProvider clears the cached provider so the next ensureProvider call
// re-runs discovery synchronously. Used when id_token verification fails — a
// signature error almost always means the IdP has rotated JWKS keys and the
// cached Verifier is rejecting tokens signed with the new key.
func (c *Client) invalidateProvider() {
	c.mu.Lock()
	c.provider = nil
	c.providerFetched = time.Time{}
	c.mu.Unlock()
}

func (c *Client) oauthConfig(p *oidc.Provider) oauth2.Config {
	return oauth2.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  c.cfg.RedirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
}

// AuthCodeURL builds the IdP authorization redirect for the given state,
// nonce, and PKCE verifier.
func (c *Client) AuthCodeURL(ctx context.Context, state, nonce, pkceVerifier string) (string, error) {
	p, err := c.ensureProvider(ctx)
	if err != nil {
		return "", err
	}
	oc := c.oauthConfig(p)
	return oc.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(pkceVerifier),
	), nil
}

// Exchange redeems the authorization code (proving possession of the PKCE
// verifier), validates the returned ID token (signature, issuer, audience,
// expiry, nonce), and extracts the identity claims.
//
// Each outbound call (code exchange, ID token verification) runs under its own
// bounded deadline so a hung IdP cannot pin the handler goroutine past
// externalTimeout. If ID token verification fails with what looks like a stale
// JWKS cache, the cached provider is invalidated and the caller can retry.
func (c *Client) Exchange(ctx context.Context, code, pkceVerifier, nonce string) (*Identity, error) {
	p, err := c.ensureProvider(ctx)
	if err != nil {
		return nil, err
	}
	oc := c.oauthConfig(p)

	// Bound code exchange. The IdP may be slow; the caller's ctx may be much
	// longer (e.g. an fx root ctx). The deferred cancel propagates as a
	// context.Canceled to oauth2 if the parent is cancelled mid-flight.
	exchCtx, cancel := context.WithTimeout(ctx, externalTimeout)
	token, err := oc.Exchange(exchCtx, code, oauth2.VerifierOption(pkceVerifier))
	cancel()
	if err != nil {
		return nil, fmt.Errorf("sso: code exchange failed: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, errors.New("sso: token response did not include an id_token")
	}

	// Bound verification too — it fetches JWKS under the hood.
	verifyCtx, cancel := context.WithTimeout(ctx, externalTimeout)
	idToken, err := p.Verifier(&oidc.Config{ClientID: c.cfg.ClientID}).Verify(verifyCtx, rawIDToken)
	cancel()
	if err != nil {
		// Signature verification failure almost always means the IdP has
		// rotated JWKS keys. Invalidate the cached provider so the caller's
		// retry (or the next user's login) re-runs discovery.
		c.invalidateProvider()
		return nil, fmt.Errorf("sso: id_token verification failed: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return nil, errors.New("sso: id_token nonce mismatch")
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("sso: parse id_token claims: %w", err)
	}

	return &Identity{
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		DisplayName:   claims.Name,
	}, nil
}
