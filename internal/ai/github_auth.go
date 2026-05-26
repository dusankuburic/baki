package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const githubClientID = "Ov23liihCNhg6umzjTqT"
const githubDeviceCodeURL = "https://github.com/login/device/code"
const githubTokenURL = "https://github.com/login/oauth/access_token"
const githubUserURL = "https://api.github.com/user"

type GitHubAuth struct {
	client *http.Client
}

func NewGitHubAuth() *GitHubAuth {
	return &GitHubAuth{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

type DeviceAuthResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type GitHubAuthResult struct {
	Status string `json:"status"`
	Token  string `json:"token,omitempty"`
	Error  string `json:"error,omitempty"`
}

type GitHubUser struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

func (g *GitHubAuth) StartDeviceFlow(ctx context.Context) (*DeviceAuthResponse, error) {
	data := url.Values{
		"client_id": {githubClientID},
		"scope":     {""},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", githubDeviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.client.Do(req)
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

func (g *GitHubAuth) PollToken(ctx context.Context, deviceCode string) (*GitHubAuthResult, error) {
	data := url.Values{
		"client_id":   {githubClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", githubTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.client.Do(req)
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

func (g *GitHubAuth) GetUser(ctx context.Context, token string) (*GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", githubUserURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get github user: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get github user failed (status %d)", resp.StatusCode)
	}

	var user GitHubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, err
	}
	return &user, nil
}
