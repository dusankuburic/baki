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
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Authenticator implements the MSAL OAuth2 device-code flow against Azure AD,
// caching the resulting access token until near-expiry. The HTTP client and
// endpoint base are injectable so the request shape is unit-testable without a
// real tenant.
type Authenticator struct {
	tenantID string
	clientID string
	scope    string
	http     *http.Client

	// deviceCodeURL / tokenURL derived from tenantID; overridable for tests.
	deviceCodeURL string
	tokenURL      string

	mu    sync.Mutex
	token *tokenCache
}

type tokenCache struct {
	accessToken string
	expiresAt   time.Time
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
func NewAuthenticator(tenantID, clientID, scope string, hc *http.Client) *Authenticator {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	base := "https://login.microsoftonline.com/" + tenantID
	return &Authenticator{
		tenantID:      tenantID,
		clientID:      clientID,
		scope:         scope,
		http:          hc,
		deviceCodeURL: base + "/oauth2/v2.0/devicecode",
		tokenURL:      base + "/oauth2/v2.0/token",
	}
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

// PollToken exchanges a device code for an access token, returning
// authorization_pending until the user completes the flow. On success the token
// is cached (see AccessToken).
func (a *Authenticator) PollToken(ctx context.Context, deviceCode string) (*AuthResult, error) {
	data := url.Values{
		"client_id":   {a.clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token poll: %w", err)
	}
	defer resp.Body.Close()
	// MSAL returns 200 with an "error":"authorization_pending" while waiting,
	// and 200 with access_token on success; 400 only for hard errors.
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if body.AccessToken != "" {
		a.mu.Lock()
		a.token = &tokenCache{accessToken: body.AccessToken, expiresAt: time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)}
		a.mu.Unlock()
		return &AuthResult{Status: "success", AccessToken: body.AccessToken, ExpiresIn: body.ExpiresIn}, nil
	}
	if body.Error == "authorization_pending" || body.Error == "slow_down" {
		return &AuthResult{Status: "pending", Error: body.Error}, nil
	}
	if body.Error != "" {
		return &AuthResult{Status: "error", Error: body.Error}, nil
	}
	return nil, fmt.Errorf("token poll: unexpected response (status %d)", resp.StatusCode)
}

// AccessToken returns a cached token if still valid (with a 60s margin), else "".
func (a *Authenticator) AccessToken() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == nil || time.Until(a.token.expiresAt) < 60*time.Second {
		return ""
	}
	return a.token.accessToken
}
