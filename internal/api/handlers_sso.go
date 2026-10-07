package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/sso"
	"pad-core/logger"
)

// SSOClient abstracts the OIDC relying party (internal/sso.Client) so handler
// tests can substitute a fake IdP interaction.
type SSOClient interface {
	ProviderName() string
	AuthCodeURL(ctx context.Context, state, nonce, pkceVerifier string) (string, error)
	Exchange(ctx context.Context, code, pkceVerifier, nonce string) (*sso.Identity, error)
}

// IdentityStore is the optional storage capability for SSO identity links, implemented by the
// Postgres backend; the service.IdentityService owns it.
type IdentityStore = service.IdentityLinkStore

// ssoFlowCookie carries the per-login CSRF state, OIDC nonce and PKCE verifier between the /start
// redirect and the /callback, signed (auth.Manager.IssueSSOFlow) so the browser holding it cannot
// change its link target. HttpOnly — the SPA never sees it.
const ssoFlowCookie = "pad_sso_flow"

func (h *AuthHandler) ssoEnabled() bool {
	return h.security.JWTEnabled && h.ssoClient != nil && h.identity.Available() && h.backend != nil
}

// handleSSOInfo tells the SPA whether to render the SSO login button.
// @Summary      SSO provider info
// @Description  handleSSOInfo tells the SPA whether to render the SSO login button.
// @Tags         sso
// @Produce      json
// @Success      200 {object} map[string]interface{} "SSO config"
// @Router       /api/auth/sso/info [get]
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
// @Summary      Start SSO login
// @Description  handleSSOStart begins the OIDC authorization-code flow: it stores the state/nonce/PKCE-verifier in an HttpOnly cookie and redirects the browser to the IdP.
// @Tags         sso
// @Produce      json
// @Success      302 {string} string "Redirect"
// @Router       /api/auth/sso/start [get]
func (h *AuthHandler) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	if !h.ssoEnabled() {
		render.Error(w, fmt.Errorf("SSO is not configured"), http.StatusNotFound)
		return
	}
	metrics.RecordAuthOp("sso_start")
	authURL, ok := h.beginSSOFlow(w, r, "")
	if !ok {
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// beginSSOFlow mints the per-attempt state/nonce/PKCE verifier into the HttpOnly flow cookie and
// returns the IdP authorization URL; linkUserID turns the callback into an account-link. On failure
// the error response has been written and ok is false.
func (h *AuthHandler) beginSSOFlow(w http.ResponseWriter, r *http.Request, linkUserID string) (string, bool) {
	// State, Nonce and a PKCE verifier (two tokens = 86 chars, within PKCE's 43-128 bounds). Any
	// crypto/rand failure fails the request rather than weakening the login flow.
	toks, err := randomTokens(4)
	if err != nil {
		render.Error(w, fmt.Errorf("failed to initialize login: %w", err), http.StatusInternalServerError)
		return "", false
	}
	flow := auth.SSOFlow{
		State:      toks[0],
		Nonce:      toks[1],
		Verifier:   toks[2] + toks[3],
		LinkUserID: linkUserID,
	}
	authURL, err := h.ssoClient.AuthCodeURL(r.Context(), flow.State, flow.Nonce, flow.Verifier)
	if err != nil {
		render.Error(w, err, http.StatusBadGateway)
		return "", false
	}
	signed, err := h.security.AuthMgr.IssueSSOFlow(flow)
	if err != nil {
		render.Error(w, fmt.Errorf("failed to initialize login: %w", err), http.StatusInternalServerError)
		return "", false
	}

	// #nosec G124 -- HttpOnly+SameSite are always set; Secure is set whenever the
	// request is TLS (it can't be unconditional for local HTTP dev).
	http.SetCookie(w, &http.Cookie{
		Name:     ssoFlowCookie,
		Value:    signed,
		Path:     "/api/auth/sso",
		MaxAge:   int(auth.SSOFlowTTL.Seconds()),
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
	return authURL, true
}

// handleSSOLinkStart begins an IdP round trip that LINKS the asserted identity to the caller's
// existing account — the step resolveSSOUser points users at. Called as an authenticated XHR (a
// top-level navigation cannot carry the bearer token): the SPA receives the IdP URL and navigates.
// @Summary      Start linking an SSO identity to the current account
// @Tags         sso
// @Produce      json
// @Success      200 {object} map[string]string "IdP authorization URL"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      404 {object} map[string]string "SSO not configured"
// @Router       /api/auth/sso/link/start [post]
func (h *AuthHandler) handleSSOLinkStart(w http.ResponseWriter, r *http.Request) {
	if !h.ssoEnabled() {
		render.Error(w, fmt.Errorf("SSO is not configured"), http.StatusNotFound)
		return
	}
	userID := h.security.CallerID(r)
	if userID == "" {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	metrics.RecordAuthOp("sso_link_start")
	authURL, ok := h.beginSSOFlow(w, r, userID)
	if !ok {
		return
	}
	render.JSON(w, map[string]string{"url": authURL})
}

// identityLinkView is the client-facing shape of a link: the opaque IdP
// subject stays server-side.
type identityLinkView struct {
	Provider  string    `json:"provider"`
	Email     string    `json:"email,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// @Summary      List the caller's linked SSO identities
// @Tags         sso
// @Produce      json
// @Success      200 {array} identityLinkView "Links"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Router       /api/auth/sso/links [get]
func (h *AuthHandler) handleSSOLinks(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	if userID == "" {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	if !h.identity.Available() {
		render.JSON(w, []identityLinkView{})
		return
	}
	links, err := h.identity.Links(r.Context(), userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	out := make([]identityLinkView, 0, len(links))
	for _, l := range links {
		out = append(out, identityLinkView{Provider: l.Provider, Email: l.Email, CreatedAt: l.CreatedAt})
	}
	render.JSON(w, out)
}

// handleSSOUnlink removes the caller's link for one provider. Refused when the account has no
// password: the link is then its only way to sign in and removing it would lock the user out.
// @Summary      Unlink an SSO identity from the current account
// @Tags         sso
// @Param        provider path string true "Provider name"
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      409 {object} map[string]string "No password set — unlinking would lock the account"
// @Router       /api/auth/sso/links/{provider} [delete]
func (h *AuthHandler) handleSSOUnlink(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	if userID == "" {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	if !h.identity.Available() || h.backend == nil {
		render.Error(w, fmt.Errorf("SSO is not configured"), http.StatusNotFound)
		return
	}
	provider := chi.URLParam(r, "provider")
	if err := h.identity.Unlink(r.Context(), userID, provider); err != nil {
		if errors.Is(err, service.ErrUnlinkOnlySignIn) {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionSSOUnlink, ResourceType: "user", ResourceID: userID, Meta: map[string]string{"provider": provider}})
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleSSOCallback receives the IdP redirect, validates state, exchanges the
// code for a verified identity, finds or creates the local user, and bounces
// the browser back to the SPA with a short-lived single-use exchange ticket
// in the URL fragment (fragments are not sent to servers or logged).
// @Summary      SSO provider callback
// @Description  handleSSOCallback receives the IdP redirect, validates state, exchanges the code for a verified identity, finds or creates the local user, and bounces the browser back to the SPA with a short-lived single-use exchange ticket in the URL fragment (fragments are not sent to servers or logged).
// @Tags         sso
// @Produce      json
// @Success      302 {string} string "Redirect"
// @Router       /api/auth/sso/callback [get]
func (h *AuthHandler) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	if !h.ssoEnabled() {
		render.Error(w, fmt.Errorf("SSO is not configured"), http.StatusNotFound)
		return
	}
	metrics.RecordAuthOp("sso_callback")

	// Always clear the flow cookie — each cookie is good for one attempt.
	// #nosec G124 -- HttpOnly+SameSite always set; Secure tracks request TLS.
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

	flow, err := h.readSSOFlowCookie(r)
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

	if flow.LinkUserID != "" {
		// Account-link round trip (handleSSOLinkStart): the caller proved the local account at
		// start and has now proved control of the IdP identity — attach, do not log in.
		if err := h.identity.Link(r.Context(), h.ssoClient.ProviderName(), flow.LinkUserID, ident); err != nil {
			logger.FromContext(r.Context()).Warn("sso: identity link failed", "err", err)
			redirectSSOError(w, r, "could not link this identity — it may already belong to another account")
			return
		}
		h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionSSOLink, ResourceType: "user", ResourceID: flow.LinkUserID, Meta: map[string]string{"provider": h.ssoClient.ProviderName()}})
		http.Redirect(w, r, "/#ssoLinked="+url.QueryEscape(h.ssoClient.ProviderName()), http.StatusFound)
		return
	}

	user, err := h.identity.ResolveSSOUser(r.Context(), h.ssoClient.ProviderName(), ident)
	if err != nil {
		// B1.8: backend/driver error text must not ride a browser redirect.
		logger.FromContext(r.Context()).Warn("sso: user resolution failed", "err", err)
		redirectSSOError(w, r, "account resolution failed")
		return
	}
	recordUser(r, user.ID, user.Email, user.Role)

	ticket, err := h.security.AuthMgr.IssueSSOTicket(user.ID, user.Email, user.Role)
	if err != nil {
		redirectSSOError(w, r, "failed to issue login ticket")
		return
	}
	http.Redirect(w, r, "/#ssoTicket="+url.QueryEscape(ticket), http.StatusFound)
}

// handleSSOExchange swaps a single-use SSO ticket for a regular token pair —
// the SSO equivalent of handleAuthLogin's response.
// @Summary      Exchange SSO ticket for JWT
// @Description  handleSSOExchange swaps a single-use SSO ticket for a regular token pair — the SSO equivalent of handleAuthLogin's response.
// @Tags         sso
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/auth/sso/exchange [post]
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
	user, err := h.authSvc.User(r.Context(), claims.UserID)
	if err != nil {
		render.Error(w, fmt.Errorf("user not found"), http.StatusUnauthorized)
		return
	}
	recordUser(r, user.ID, user.Email, user.Role)

	pair, err := h.security.AuthMgr.Issue(user.ID, user.Email, user.Role)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if h.sessions.Enabled() {
		ua, ip := r.UserAgent(), middleware.ClientIP(r, h.security.TrustedProxies)
		if err := h.sessions.Record(r.Context(), pair.RefreshID, user.ID, pair.RefreshExpiresAt, ua, ip); err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
	}
	// SSO sessions are always persistent (the web client treats them as
	// remembered): set the same HttpOnly refresh cookie login sets, so the web
	// build can drop its localStorage copy of the token.
	setRefreshCookie(w, r, pair.RefreshToken, pair.RefreshExpiresAt, true)

	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionSSOLogin, ResourceType: "user", ResourceID: user.ID, Meta: map[string]string{"provider": h.ssoClient.ProviderName()}})
	// Mirrors handleAuthLogin: the client's LoginResponseSchema requires the
	// `user` object alongside the tokens.
	render.JSON(w, map[string]any{
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		},
		"accessToken":      pair.AccessToken,
		"refreshToken":     pair.RefreshToken,
		"expiresAt":        pair.ExpiresAt,
		"refreshId":        pair.RefreshID,
		"refreshExpiresAt": pair.RefreshExpiresAt,
	})
}

// readSSOFlowCookie returns the flow /start signed into the cookie; a cookie that is missing, expired or not
// signed by this deployment is refused.
func (h *AuthHandler) readSSOFlowCookie(r *http.Request) (*auth.SSOFlow, error) {
	c, err := r.Cookie(ssoFlowCookie)
	if err != nil {
		return nil, err
	}
	return h.security.AuthMgr.VerifySSOFlow(c.Value)
}

// redirectSSOError sends the browser back to the SPA with a human-readable error in the URL
// fragment. Deliberately a redirect, not an error page: the user is mid-login in a browser.
func redirectSSOError(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/#ssoError="+url.QueryEscape(msg), http.StatusFound)
}

// requestIsTLS reports whether the request reached us over HTTPS, directly or
// via a TLS-terminating proxy.
func requestIsTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// randomToken returns a 43-character URL-safe random string (256 bits); it errors rather than
// panicking on crypto/rand failure so the handler can fail the request cleanly.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("sso: crypto/rand unavailable: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomTokens returns n independent random tokens, failing fast if crypto/rand
// is unavailable.
func randomTokens(n int) ([]string, error) {
	toks := make([]string, n)
	for i := range toks {
		t, err := randomToken()
		if err != nil {
			return nil, err
		}
		toks[i] = t
	}
	return toks, nil
}

// registerSSORoutes wires the OIDC SSO sub-routes: the login round trip plus
// authenticated account linking.
func (h *AuthHandler) registerSSORoutes(rb *route.Builder) {
	rb.Route("/sso", func(rb *route.Builder) {
		// The login round trip, before the browser has any token. exchange
		// carries its own single-use ticket and rides the auth bucket.
		rb.Group(func(rb *route.Builder) {
			rb.Get("/info", h.handleSSOInfo)
			rb.Get("/start", h.handleSSOStart)
			rb.Get("/callback", h.handleSSOCallback)
			rb.Post("/exchange", h.handleSSOExchange, route.RateLimit(route.RateAuth))
		}, route.Public())
		// Account linking (authenticated): start a link round trip, list
		// and remove the caller's linked identities.
		rb.Post("/link/start", h.handleSSOLinkStart)
		rb.Get("/links", h.handleSSOLinks)
		rb.Delete("/links/{provider}", h.handleSSOUnlink)
	})
}
