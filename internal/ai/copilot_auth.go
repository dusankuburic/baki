package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const copilotClientID = "Iv1.b507a08c87ecfe98" // #nosec G101 -- public OAuth client ID, not a secret

var copilotTokenExchangeURL = "https://api.github.com/copilot_internal/v2/token" // #nosec G101 -- public API endpoint URL, not a credential

// CopilotSessionToken holds a short-lived Copilot API session token (~30 min).
type CopilotSessionToken struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// CopilotAuth handles the two-step Copilot authentication:
// 1. GitHub OAuth device flow (with Copilot's VS Code client ID)
// 2. Copilot session token exchange and caching
type CopilotAuth struct {
	client        *http.Client
	mu            sync.Mutex
	sessionTokens map[string]*CopilotSessionToken
	flight        singleflight.Group
}

func NewCopilotAuth() *CopilotAuth {
	return &CopilotAuth{client: authHTTPClient, sessionTokens: make(map[string]*CopilotSessionToken)}
}

// StartDeviceFlow initiates the GitHub OAuth device flow using the Copilot client ID.
func (a *CopilotAuth) StartDeviceFlow(ctx context.Context) (*DeviceAuthResponse, error) {
	data := url.Values{
		"client_id": {copilotClientID},
		"scope":     {""},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", githubDeviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code request failed (status %d)", resp.StatusCode)
	}

	var result DeviceAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode device code response: %w", err)
	}
	return &result, nil
}

// PollToken polls for the GitHub access token using the Copilot client ID.
func (a *CopilotAuth) PollToken(ctx context.Context, deviceCode string) (*GitHubAuthResult, error) {
	data := url.Values{
		"client_id":   {copilotClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", githubTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token poll request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token poll request failed (status %d)", resp.StatusCode)
	}

	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if result.Error == "authorization_pending" {
		return &GitHubAuthResult{Status: "pending"}, nil
	}
	if result.Error == "slow_down" {
		return &GitHubAuthResult{Status: "pending"}, nil
	}
	if result.Error != "" {
		return &GitHubAuthResult{Status: "error", Error: result.Error}, nil
	}

	return &GitHubAuthResult{Status: "success", Token: result.AccessToken}, nil
}

// ExchangeToken exchanges a long-lived GitHub OAuth token for a short-lived Copilot session token.
func (a *CopilotAuth) ExchangeToken(ctx context.Context, githubToken string) (*CopilotSessionToken, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", copilotTokenExchangeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build token exchange request: %w", err)
	}
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", copilotEditorVersion)
	req.Header.Set("Editor-Plugin-Version", copilotPluginVersion)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("GitHub token is invalid or expired — please re-authenticate")
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("no active Copilot subscription found for this GitHub account")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot token exchange failed (status %d)", resp.StatusCode)
	}

	var token CopilotSessionToken
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return nil, fmt.Errorf("decode token exchange response: %w", err)
	}
	if token.Token == "" {
		return nil, fmt.Errorf("copilot token exchange returned empty token")
	}
	return &token, nil
}

// GetSessionToken returns a valid Copilot session token, refreshing if expired or within 5 min of expiry.
// Uses singleflight so concurrent callers with the same GitHub token share a single HTTP exchange,
// while different tokens proceed independently. The mutex is never held during network I/O.
// The cache is keyed by a hash of the GitHub token so multiple users in cloud mode don't thrash
// each other's cached session token.
func (a *CopilotAuth) GetSessionToken(ctx context.Context, githubToken string) (string, error) {
	cacheKey := tokenHash(githubToken)
	a.mu.Lock()
	if a.sessionTokens == nil {
		a.sessionTokens = make(map[string]*CopilotSessionToken)
	}
	now := time.Now().Unix()
	// Sweep expired entries so users who never return don't accumulate forever.
	for k, v := range a.sessionTokens {
		if v.ExpiresAt <= now {
			delete(a.sessionTokens, k)
		}
	}
	if st := a.sessionTokens[cacheKey]; st != nil && st.ExpiresAt > now+300 {
		token := st.Token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()

	v, err, _ := a.flight.Do(githubToken, func() (any, error) {
		return a.ExchangeToken(ctx, githubToken)
	})
	if err != nil {
		return "", err
	}
	token := v.(*CopilotSessionToken)

	a.mu.Lock()
	a.sessionTokens[cacheKey] = token
	a.mu.Unlock()
	return token.Token, nil
}

// tokenHash returns a deterministic, non-reversible key for the session-token cache.
// The GitHub token is a long-lived OAuth secret — never store or log it as a bare map key.
func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:16])
}
