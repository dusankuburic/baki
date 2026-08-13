// Package padcloud implements the optional Power Platform connector that
// ingests desktop flows from a customer's environment into the library, so the
// governance surface (portfolio, scanner, drift alerts) covers live flows
// instead of only manually-uploaded exports.
//
// Architecture (each unknown sits behind an interface so it can evolve / be
// mocked independently):
//
//   - Auth: MSAL device-flow against Azure AD (StartDeviceFlow / PollToken /
//     cached token). Mirrors the proven internal/ai/github_auth device-flow.
//   - Client: enumerates desktop flows + fetches a definition (Power Platform
//     API / Dataverse). HTTP impl, endpoints documented for validation.
//   - Converter: the format bridge — PAD's cloud JSON action schema → the
//     parser's models.FlowDocument. THE de-risking interface; a concrete impl
//     must be validated against a real API sample.
//   - Ingester: orchestrates list → fetch → convert → store.
//
// The connector is feature-flagged (PowerPlatformConfig): all of tenant/client/
// environment/scope must be set, and IngestInterval gates the periodic pull.
// With any absent the connector is a no-op.
package padcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenStore optionally persists the OAuth token so it survives process
// restarts. Without a store (nil) the authenticator is in-memory only —
// backward-compatible with the original behaviour. With a DB-backed store,
// the access+refresh token is loaded on startup and saved on every refresh.
type TokenStore interface {
	LoadToken(ctx context.Context) (*StoredToken, error)
	SaveToken(ctx context.Context, t *StoredToken) error
}

// StoredToken is the persisted token shape (access + refresh + expiry).
type StoredToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Authenticator implements the MSAL OAuth2 device-code flow against Azure AD,
// caching the resulting access token until near-expiry. The HTTP client and
// endpoint base are injectable so the request shape is unit-testable without a
// real tenant.
type Authenticator struct {
	tenantID string
	clientID string
	scope    string
	http     *http.Client
	store    TokenStore // optional persistence (nil = in-memory only)

	// deviceCodeURL / tokenURL derived from tenantID; overridable for tests.
	deviceCodeURL string
	tokenURL      string

	mu    sync.Mutex
	token *tokenCache

	// refreshMu single-flights token refresh so concurrent callers don't each
	// exchange the refresh token. MSAL rotates the refresh token on use, so a
	// concurrent double-refresh would leave one caller persisting a stale,
	// already-rotated token.
	refreshMu sync.Mutex
}

type tokenCache struct {
	accessToken  string
	refreshToken string
	expiresAt    time.Time
}

// DeviceAuthResponse mirrors the MSAL device-code endpoint response.
type DeviceAuthResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Message         string `json:"message"`
}

// NewAuthenticator builds an Authenticator for the given tenant + app + scope.
// store is optional (nil = in-memory only, backward-compatible).
func NewAuthenticator(tenantID, clientID, scope string, hc *http.Client, store TokenStore) *Authenticator {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	base := "https://login.microsoftonline.com/" + tenantID
	return &Authenticator{
		tenantID:      tenantID,
		clientID:      clientID,
		scope:         scope,
		http:          hc,
		store:         store,
		deviceCodeURL: base + "/oauth2/v2.0/devicecode",
		tokenURL:      base + "/oauth2/v2.0/token",
	}
}

// LoadCachedToken restores a persisted token from the store on startup. Call
// once after construction (before the first sweep) so a process restart
// doesn't lose auth. No-op when no store is configured.
func (a *Authenticator) LoadCachedToken(ctx context.Context) error {
	if a.store == nil {
		return nil
	}
	st, err := a.store.LoadToken(ctx)
	if err != nil {
		return fmt.Errorf("load cached token: %w", err)
	}
	if st == nil || st.AccessToken == "" {
		return nil // nothing stored
	}
	a.mu.Lock()
	a.token = &tokenCache{
		accessToken:  st.AccessToken,
		refreshToken: st.RefreshToken,
		expiresAt:    st.ExpiresAt,
	}
	a.mu.Unlock()
	return nil
}

// StartDeviceFlow initiates the device-code flow, returning the code the user
// enters at the verification URI.
func (a *Authenticator) StartDeviceFlow(ctx context.Context) (*DeviceAuthResponse, error) {
	data := url.Values{
		"client_id": {a.clientID},
		"scope":     {a.scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.deviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build device-code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device-code request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device-code request failed (status %d)", resp.StatusCode)
	}
	var out DeviceAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode device-code response: %w", err)
	}
	return &out, nil
}

// AuthResult is the outcome of polling for a token.
type AuthResult struct {
	Status      string
	AccessToken string
	ExpiresIn   int
	Error       string
}

// tokenResponse is the MSAL token-endpoint response shape shared by the
// device-code poll and refresh_token grants.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

// postToken POSTs form-encoded data to the token endpoint and decodes the JSON
// response. A decode failure is returned as an error carrying the HTTP status
// (previously both call sites silently ignored it, collapsing a malformed body
// — e.g. an HTML error page from an in-path proxy — into a generic
// "unexpected response").
func (a *Authenticator) postToken(ctx context.Context, data url.Values) (*tokenResponse, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var body tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode token response (status %d): %w", resp.StatusCode, err)
	}
	return &body, resp.StatusCode, nil
}

// PollToken exchanges a device code for an access token, returning
// authorization_pending until the user completes the flow. On success the token
// is cached (see AccessToken).
func (a *Authenticator) PollToken(ctx context.Context, deviceCode string) (*AuthResult, error) {
	data := url.Values{
		"client_id":   {a.clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	// MSAL returns 200 with an "error":"authorization_pending" while waiting,
	// and 200 with access_token on success; 400 only for hard errors.
	body, status, err := a.postToken(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("token poll: %w", err)
	}

	if body.AccessToken != "" {
		exp := time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
		a.mu.Lock()
		a.token = &tokenCache{accessToken: body.AccessToken, refreshToken: body.RefreshToken, expiresAt: exp}
		a.mu.Unlock()
		a.saveToken(context.Background(), body.AccessToken, body.RefreshToken, exp)
		return &AuthResult{Status: "success", AccessToken: body.AccessToken, ExpiresIn: body.ExpiresIn}, nil
	}
	if body.Error == "authorization_pending" || body.Error == "slow_down" {
		return &AuthResult{Status: "pending", Error: body.Error}, nil
	}
	if body.Error != "" {
		return &AuthResult{Status: "error", Error: body.Error}, nil
	}
	return nil, fmt.Errorf("token poll: unexpected response (status %d)", status)
}

// AccessToken returns a valid access token, refreshing transparently when the
// cached token is near-expiry and a refresh token is available. Returns "" if
// no token is cached, the refresh token is absent, or refresh failed — the
// caller (client.go) surfaces "complete device-flow auth first" so the operator
// knows to re-authenticate.
func (a *Authenticator) AccessToken() string {
	a.mu.Lock()
	if a.token == nil {
		a.mu.Unlock()
		return ""
	}
	if time.Until(a.token.expiresAt) >= 60*time.Second {
		tok := a.token.accessToken
		a.mu.Unlock()
		return tok
	}
	refreshToken := a.token.refreshToken
	a.mu.Unlock()

	if refreshToken == "" {
		return "" // can't refresh — need manual re-auth
	}

	// Single-flight the refresh: only one goroutine exchanges the (rotating)
	// refresh token at a time. Waiters re-check the cached token after acquiring
	// the lock, since the winner may have already refreshed it.
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	a.mu.Lock()
	if a.token != nil && time.Until(a.token.expiresAt) >= 60*time.Second {
		tok := a.token.accessToken
		a.mu.Unlock()
		return tok // another goroutine refreshed while we waited
	}
	a.mu.Unlock()

	// Attempt a refresh. Use a background context (not tied to any single
	// request) since this is a lifecycle operation.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.refresh(ctx); err != nil {
		// Multi-replica rotation race: MSAL rotates the refresh token on each
		// use, so in a multi-replica deploy another replica may have already
		// refreshed (and persisted) the token, leaving this replica's in-memory
		// copy stale → invalid_grant. Reload the latest token from the shared
		// store and retry once before giving up — without this self-heal, a
		// replica stays broken (every API call fails) until it is restarted or
		// an admin re-runs the device flow.
		if a.store == nil {
			return "" // no store to recover from — need manual re-auth
		}
		if reloadErr := a.LoadCachedToken(ctx); reloadErr != nil {
			return ""
		}
		if err := a.refresh(ctx); err != nil {
			return "" // refresh still failing after reload — need manual re-auth
		}
	}
	a.mu.Lock()
	tok := a.token.accessToken
	a.mu.Unlock()
	return tok
}

// refresh exchanges the cached refresh token for a new access+refresh token
// pair. MSAL rotates the refresh token on each use, so the new refresh token
// must be persisted for the next cycle.
func (a *Authenticator) refresh(ctx context.Context) error {
	a.mu.Lock()
	rt := a.token.refreshToken
	a.mu.Unlock()
	if rt == "" {
		return fmt.Errorf("no refresh token")
	}
	data := url.Values{
		"client_id":     {a.clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt},
		"scope":         {a.scope},
	}
	body, status, err := a.postToken(ctx, data)
	if err != nil {
		return fmt.Errorf("refresh token: %w", err)
	}
	if body.AccessToken == "" {
		if body.Error != "" {
			return fmt.Errorf("refresh token: %s", body.Error)
		}
		return fmt.Errorf("refresh token: unexpected response (status %d)", status)
	}
	// MSAL may not return a new refresh_token (some configs reuse the old one).
	// Keep the old one if absent so the next refresh can still try.
	newRefresh := body.RefreshToken
	if newRefresh == "" {
		newRefresh = rt
	}
	exp := time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	a.mu.Lock()
	a.token = &tokenCache{accessToken: body.AccessToken, refreshToken: newRefresh, expiresAt: exp}
	a.mu.Unlock()
	a.saveToken(context.Background(), body.AccessToken, newRefresh, exp)
	return nil
}

// saveToken persists the token to the store (if configured). Errors are logged
// but not surfaced — a failed save shouldn't block the in-memory token from
// being used for this sweep.
func (a *Authenticator) saveToken(ctx context.Context, access, refresh string, exp time.Time) {
	if a.store == nil {
		return
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := a.store.SaveToken(ctx2, &StoredToken{AccessToken: access, RefreshToken: refresh, ExpiresAt: exp}); err != nil {
		// Best-effort: the in-memory token is still valid for this process.
		slog.Warn("padcloud: failed to persist token", "error", err)
	}
}
