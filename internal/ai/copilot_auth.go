package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

// CopilotAuth handles Copilot authentication: the GitHub OAuth device flow
// (Copilot's VS Code client ID) and the session-token exchange and cache.
type CopilotAuth struct {
	client        *http.Client
	mu            sync.Mutex
	sessionTokens map[string]*CopilotSessionToken
	// lastSweep is when GetSessionToken last dropped expired entries
	// (guarded by mu); see copilotTokenSweepInterval.
	lastSweep time.Time
	flight    singleflight.Group
}

// copilotTokenSweepInterval throttles the expired-entry sweep of the
// session-token map: it walked the whole map under the mutex on EVERY call,
// and tokens live ~30 minutes, so once a minute keeps the map just as bounded.
const copilotTokenSweepInterval = time.Minute

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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBodyBytes)).Decode(&result); err != nil {
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBodyBytes)).Decode(&result); err != nil {
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
	// A success implies a stored token: a 200 with neither a token nor an error must stop the client, not
	// switch it to a provider with no credential.
	if result.AccessToken == "" {
		return &GitHubAuthResult{Status: "error", Error: "GitHub answered without an access token"}, nil
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBodyBytes)).Decode(&token); err != nil {
		return nil, fmt.Errorf("decode token exchange response: %w", err)
	}
	if token.Token == "" {
		return nil, fmt.Errorf("copilot token exchange returned empty token")
	}
	return &token, nil
}

// GetSessionToken returns a valid session token, refreshing within 5 min of
// expiry. singleflight shares one exchange per GitHub token (keyed by the
// token's hash — the raw token never sits in the map); the exchange runs
// detached from the callers' cancellation, mirroring entraTokenSource.Token,
// so one joiner's ended ctx cannot fail the shared exchange, and
// authHTTPClient's 30 s timeout bounds it. The mutex is never held during I/O.
func (a *CopilotAuth) GetSessionToken(ctx context.Context, githubToken string) (string, error) {
	cacheKey := tokenHash(githubToken)
	a.mu.Lock()
	if a.sessionTokens == nil {
		a.sessionTokens = make(map[string]*CopilotSessionToken)
	}
	wall := time.Now()
	now := wall.Unix()
	// Sweep expired entries so users who never return don't accumulate
	// forever — at most once per copilotTokenSweepInterval.
	if wall.Sub(a.lastSweep) >= copilotTokenSweepInterval {
		a.lastSweep = wall
		for k, v := range a.sessionTokens {
			if v.ExpiresAt <= now {
				delete(a.sessionTokens, k)
			}
		}
	}
	if st := a.sessionTokens[cacheKey]; st != nil && st.ExpiresAt > now+300 {
		token := st.Token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()

	ch := a.flight.DoChan(cacheKey, func() (any, error) {
		tok, err := a.ExchangeToken(context.WithoutCancel(ctx), githubToken)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.sessionTokens[cacheKey] = tok
		a.mu.Unlock()
		return tok.Token, nil
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// tokenHash returns a deterministic, non-reversible key for the session-token cache.
// The GitHub token is a long-lived OAuth secret — never store or log it as a bare map key.
func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:16])
}
