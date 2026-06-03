package ai

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

const copilotClientID = "Iv1.b507a08c87ecfe98"
var copilotTokenExchangeURL = "https://api.github.com/copilot_internal/v2/token"

// CopilotSessionToken holds a short-lived Copilot API session token (~30 min).
type CopilotSessionToken struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// CopilotAuth handles the two-step Copilot authentication:
// 1. GitHub OAuth device flow (with Copilot's VS Code client ID)
// 2. Copilot session token exchange and caching
type CopilotAuth struct {
	client       *http.Client
	mu           sync.Mutex
	sessionToken *CopilotSessionToken
}

func NewCopilotAuth() *CopilotAuth {
	return &CopilotAuth{client: authHTTPClient}
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
func (a *CopilotAuth) GetSessionToken(ctx context.Context, githubToken string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now().Unix()
	if a.sessionToken != nil && a.sessionToken.ExpiresAt > now+300 {
		return a.sessionToken.Token, nil
	}

	token, err := a.ExchangeToken(ctx, githubToken)
	if err != nil {
		return "", err
	}
	a.sessionToken = token
	return token.Token, nil
}
