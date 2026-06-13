package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/sso"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// SSOClient abstracts the OIDC relying party (internal/sso.Client) so handler
// tests can substitute a fake IdP interaction.
type SSOClient interface {
	ProviderName() string
	AuthCodeURL(ctx context.Context, state, nonce, pkceVerifier string) (string, error)
	Exchange(ctx context.Context, code, pkceVerifier, nonce string) (*sso.Identity, error)
}

// IdentityStore is the optional storage capability for SSO identity links,
// implemented by the Postgres backend (same optional-capability pattern as
// RefreshTokenStore).
type IdentityStore interface {
	SaveIdentityLink(ctx context.Context, link *storageif.IdentityLink) error
	LoadIdentityLink(ctx context.Context, provider, subject string) (*storageif.IdentityLink, error)
}

// ssoFlowCookie carries the per-login CSRF state, OIDC nonce, and PKCE
// verifier between the /start redirect and the /callback. HttpOnly — the SPA
// never sees it.
const ssoFlowCookie = "pad_sso_flow"

type ssoFlowState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

func (h *AuthHandler) ssoEnabled() bool {
	return h.security.JWTEnabled && h.ssoClient != nil && h.identityStore != nil && h.backend != nil
}

// handleSSOInfo tells the SPA whether to render the SSO login button.
func (h *AuthHandler) handleSSOInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{"enabled": h.ssoEnabled()}
	if h.ssoEnabled() {
		info["provider"] = h.ssoClient.ProviderName()
	}
	render.JSON(w, info)
}

// handleSSOStart begins the OIDC authorization-code flow: it stores the
// state/nonce/PKCE-verifier in an HttpOnly cookie and redirects the browser
// to the IdP.
func (h *AuthHandler) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	if !h.ssoEnabled() {
		render.Error(w, fmt.Errorf("SSO is not configured"), http.StatusNotFound)
		return
	}
	metrics.RecordAuthOp("sso_start")

	flow := ssoFlowState{
		State:    randomToken(),
		Nonce:    randomToken(),
		Verifier: randomToken() + randomToken(), // 86 chars, within PKCE's 43-128 bounds
	}
	authURL, err := h.ssoClient.AuthCodeURL(r.Context(), flow.State, flow.Nonce, flow.Verifier)
	if err != nil {
		render.Error(w, err, http.StatusBadGateway)
		return
	}

	payload, _ := json.Marshal(flow)
	http.SetCookie(w, &http.Cookie{
		Name:     ssoFlowCookie,
		Value:    base64.RawURLEncoding.EncodeToString(payload),
		Path:     "/api/auth/sso",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleSSOCallback receives the IdP redirect, validates state, exchanges the
// code for a verified identity, finds or creates the local user, and bounces
// the browser back to the SPA with a short-lived single-use exchange ticket
// in the URL fragment (fragments are not sent to servers or logged).
func (h *AuthHandler) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	if !h.ssoEnabled() {
		render.Error(w, fmt.Errorf("SSO is not configured"), http.StatusNotFound)
		return
	}
	metrics.RecordAuthOp("sso_callback")

	// Always clear the flow cookie — each cookie is good for one attempt.
	http.SetCookie(w, &http.Cookie{
		Name: ssoFlowCookie, Value: "", Path: "/api/auth/sso", MaxAge: -1,
		HttpOnly: true, Secure: requestIsTLS(r), SameSite: http.SameSiteLaxMode,
	})

	q := r.URL.Query()
	if errCode := q.Get("error"); errCode != "" {
		// User cancelled at the IdP, consent denied, etc.
		redirectSSOError(w, r, fmt.Sprintf("identity provider returned %s", errCode))
		return
	}

	flow, err := readSSOFlowCookie(r)
	if err != nil {
		redirectSSOError(w, r, "login session expired or missing — try again")
		return
	}
	if state := q.Get("state"); state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(flow.State)) != 1 {
		redirectSSOError(w, r, "state mismatch — try again")
		return
	}
	code := q.Get("code")
	if code == "" {
		redirectSSOError(w, r, "missing authorization code")
		return
	}

	ident, err := h.ssoClient.Exchange(r.Context(), code, flow.Verifier, flow.Nonce)
	if err != nil {
		redirectSSOError(w, r, "identity verification failed")
		return
	}

	user, err := h.resolveSSOUser(r.Context(), ident)
	if err != nil {
		redirectSSOError(w, r, err.Error())
		return
	}

	ticket, err := h.security.AuthMgr.IssueSSOTicket(user.ID, user.Email, user.Role)
	if err != nil {
		redirectSSOError(w, r, "failed to issue login ticket")
		return
	}
	http.Redirect(w, r, "/#ssoTicket="+url.QueryEscape(ticket), http.StatusFound)
}

// handleSSOExchange swaps a single-use SSO ticket for a regular token pair —
// the SSO equivalent of handleAuthLogin's response.
func (h *AuthHandler) handleSSOExchange(w http.ResponseWriter, r *http.Request) {
	if !h.ssoEnabled() {
		render.Error(w, fmt.Errorf("SSO is not configured"), http.StatusNotFound)
		return
	}
	metrics.RecordAuthOp("sso_exchange")

	var req struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Ticket == "" {
		render.Error(w, fmt.Errorf("ticket is required"), http.StatusBadRequest)
		return
	}

	claims, err := h.security.AuthMgr.ConsumeSSOTicket(req.Ticket)
	if err != nil {
		render.Error(w, fmt.Errorf("invalid or expired ticket"), http.StatusUnauthorized)
		return
	}

	// Re-load the user so the issued pair reflects the current role/email even
	// if they changed between callback and exchange.
	user, err := h.backend.LoadUserByID(r.Context(), claims.UserID)
	if err != nil {
		render.Error(w, fmt.Errorf("user not found"), http.StatusUnauthorized)
		return
	}

	pair, err := h.security.AuthMgr.Issue(user.ID, user.Email, user.Role)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if h.tokenStore != nil {
		if err := h.tokenStore.StoreRefreshToken(r.Context(), pair.RefreshID, user.ID, pair.RefreshExpiresAt); err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionSSOLogin, "user", user.ID, map[string]string{"provider": h.ssoClient.ProviderName()})
	render.JSON(w, pair)
}

// resolveSSOUser maps a verified IdP identity to a local user:
//  1. an existing identity link wins;
//  2. otherwise an existing account with the same verified email is linked;
//  3. otherwise a new account is provisioned (JIT) with no usable password.
//
// Both linking and provisioning require the IdP to assert the email as
// verified — auto-linking on an unverified email would let an attacker who
// controls a sloppy IdP account take over the matching local account.
func (h *AuthHandler) resolveSSOUser(ctx context.Context, ident *sso.Identity) (*storageif.User, error) {
	provider := h.ssoClient.ProviderName()
	ident.Email = strings.ToLower(ident.Email)

	if link, err := h.identityStore.LoadIdentityLink(ctx, provider, ident.Subject); err == nil {
		return h.backend.LoadUserByID(ctx, link.UserID)
	} else if !errors.Is(err, storageif.ErrNotFound) {
		return nil, fmt.Errorf("login failed — try again")
	}

	if ident.Email == "" || !ident.EmailVerified {
		return nil, fmt.Errorf("your identity provider did not assert a verified email address")
	}

	newLink := &storageif.IdentityLink{
		Provider: provider,
		Subject:  ident.Subject,
		Email:    ident.Email,
	}

	if existing, err := h.backend.LoadUserByEmail(ctx, ident.Email); err == nil {
		newLink.UserID = existing.ID

		// Security: if the local account was NOT email_verified (e.g. created
		// via an open registration form without confirmation), it is
		// vulnerable to pre-account takeover.
		//
		// By linking it now to a verified SSO identity, we take ownership.
		// We MUST clear the existing password hash so the "shadow"
		// registration's password (known to an attacker) can no longer be
		// used to log in. This forces the account to only use the secure IdP.
		if !existing.EmailVerified {
			existing.EmailVerified = true
			existing.Password = "" // Strip the insecure password hash
			if err := h.backend.SaveUser(ctx, existing); err != nil {
				return nil, fmt.Errorf("failed to harden account — try again")
			}
			logger.Info("account takeover prevented: cleared password on unverified account linked to verified SSO identity", "email", ident.Email)
		}

		if err := h.identityStore.SaveIdentityLink(ctx, newLink); err != nil {
			return nil, fmt.Errorf("failed to link account — try again")
		}
		return existing, nil
	}

	user := &storageif.User{
		ID:    uuid.NewString(),
		Email: ident.Email,
		// No usable password: SSO-only account. bcrypt comparison against an
		// empty hash always fails, so password login is naturally impossible.
		Password:      "",
		EmailVerified: true, // IdP asserted verification
		Role:          auth.RoleMember,
		DisplayName:   ident.DisplayName,
	}
	if err := h.backend.CreateUser(ctx, user); err != nil {
		return nil, fmt.Errorf("failed to create account — try again")
	}
	newLink.UserID = user.ID
	if err := h.identityStore.SaveIdentityLink(ctx, newLink); err != nil {
		return nil, fmt.Errorf("failed to link account — try again")
	}
	return user, nil
}

func readSSOFlowCookie(r *http.Request) (*ssoFlowState, error) {
	c, err := r.Cookie(ssoFlowCookie)
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil, err
	}
	var flow ssoFlowState
	if err := json.Unmarshal(raw, &flow); err != nil {
		return nil, err
	}
	if flow.State == "" || flow.Nonce == "" || flow.Verifier == "" {
		return nil, errors.New("incomplete sso flow state")
	}
	return &flow, nil
}

// redirectSSOError sends the browser back to the SPA with a human-readable
// error in the URL fragment. Deliberately a redirect, not an error page: the
// user is mid-login in a browser, not an API client.
func redirectSSOError(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/#ssoError="+url.QueryEscape(msg), http.StatusFound)
}

// requestIsTLS reports whether the request reached us over HTTPS, directly or
// via a TLS-terminating proxy.
func requestIsTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// randomToken returns a 43-character URL-safe random string (256 bits).
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable; surface loudly rather than
		// silently weakening the login flow.
		panic(fmt.Sprintf("sso: crypto/rand unavailable: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
