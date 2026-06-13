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

// Client wraps the OIDC provider for one configured IdP. Discovery is lazy:
// the IdP is first contacted on the first login attempt, not at startup, so a
// temporarily unreachable IdP doesn't prevent the app from booting.
type Client struct {
	cfg config.SSOConfig

	mu       sync.Mutex
	provider *oidc.Provider
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

// ensureProvider performs OIDC discovery once and caches the result. On
// failure nothing is cached, so the next request retries.
func (c *Client) ensureProvider(ctx context.Context) (*oidc.Provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider != nil {
		return c.provider, nil
	}
	p, err := oidc.NewProvider(ctx, c.cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("sso: discovery for %s failed: %w", c.cfg.IssuerURL, err)
	}
	c.provider = p
	return p, nil
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
func (c *Client) Exchange(ctx context.Context, code, pkceVerifier, nonce string) (*Identity, error) {
	p, err := c.ensureProvider(ctx)
	if err != nil {
		return nil, err
	}
	oc := c.oauthConfig(p)

	token, err := oc.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return nil, fmt.Errorf("sso: code exchange failed: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, errors.New("sso: token response did not include an id_token")
	}

	idToken, err := p.Verifier(&oidc.Config{ClientID: c.cfg.ClientID}).Verify(ctx, rawIDToken)
	if err != nil {
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
