package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- StartDeviceFlow ---

func TestCopilotAuth_StartDeviceFlow_UsesCorrectClientID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("client_id"); got != copilotClientID {
			t.Errorf("client_id = %q, want %q (copilotClientID, not githubClientID)", got, copilotClientID)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DeviceAuthResponse{
			DeviceCode:      "dev-abc",
			UserCode:        "USER-XYZ",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        5,
		})
	}))
	defer server.Close()

	orig := githubDeviceCodeURL
	defer func() { githubDeviceCodeURL = orig }()
	githubDeviceCodeURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	resp, err := auth.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.DeviceCode != "dev-abc" {
		t.Errorf("DeviceCode = %q, want dev-abc", resp.DeviceCode)
	}
	if resp.UserCode != "USER-XYZ" {
		t.Errorf("UserCode = %q, want USER-XYZ", resp.UserCode)
	}
	if resp.Interval != 5 {
		t.Errorf("Interval = %d, want 5", resp.Interval)
	}
}

func TestCopilotAuth_StartDeviceFlow_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer server.Close()

	orig := githubDeviceCodeURL
	defer func() { githubDeviceCodeURL = orig }()
	githubDeviceCodeURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	_, err := auth.StartDeviceFlow(context.Background())
	if err == nil {
		t.Fatal("expected error for 503 response")
	}
}

// --- PollToken ---

func TestCopilotAuth_PollToken_Pending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
	}))
	defer server.Close()

	orig := githubTokenURL
	defer func() { githubTokenURL = orig }()
	githubTokenURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	result, err := auth.PollToken(context.Background(), "dev-code")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "pending" {
		t.Errorf("status = %q, want pending", result.Status)
	}
}

func TestCopilotAuth_PollToken_SlowDown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
	}))
	defer server.Close()

	orig := githubTokenURL
	defer func() { githubTokenURL = orig }()
	githubTokenURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	result, err := auth.PollToken(context.Background(), "dev-code")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "pending" {
		t.Errorf("slow_down should map to pending, got %q", result.Status)
	}
}

func TestCopilotAuth_PollToken_Success_UsesCorrectClientID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("client_id"); got != copilotClientID {
			t.Errorf("client_id = %q, want %q", got, copilotClientID)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"access_token": "ghp_copilot123"})
	}))
	defer server.Close()

	orig := githubTokenURL
	defer func() { githubTokenURL = orig }()
	githubTokenURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	result, err := auth.PollToken(context.Background(), "dev-code")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "success" {
		t.Errorf("status = %q, want success", result.Status)
	}
	if result.Token != "ghp_copilot123" {
		t.Errorf("token = %q, want ghp_copilot123", result.Token)
	}
}

func TestCopilotAuth_PollToken_GenericError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
	}))
	defer server.Close()

	orig := githubTokenURL
	defer func() { githubTokenURL = orig }()
	githubTokenURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	result, err := auth.PollToken(context.Background(), "dev-code")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "error" {
		t.Errorf("status = %q, want error", result.Status)
	}
	if result.Error != "access_denied" {
		t.Errorf("error = %q, want access_denied", result.Error)
	}
}

func TestCopilotAuth_PollToken_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer server.Close()

	orig := githubTokenURL
	defer func() { githubTokenURL = orig }()
	githubTokenURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	_, err := auth.PollToken(context.Background(), "dev-code")
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

// --- ExchangeToken ---

func TestCopilotAuth_ExchangeToken_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.Header.Get("Authorization") != "token gh-oauth-tok" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Editor-Version") == "" {
			t.Error("Editor-Version header missing")
		}
		if r.Header.Get("Editor-Plugin-Version") == "" {
			t.Error("Editor-Plugin-Version header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CopilotSessionToken{
			Token:     "cop_session_abc",
			ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
		})
	}))
	defer server.Close()

	orig := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = orig }()
	copilotTokenExchangeURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	tok, err := auth.ExchangeToken(context.Background(), "gh-oauth-tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.Token != "cop_session_abc" {
		t.Errorf("token = %q, want cop_session_abc", tok.Token)
	}
	if tok.ExpiresAt <= time.Now().Unix() {
		t.Error("ExpiresAt should be in the future")
	}
}

func TestCopilotAuth_ExchangeToken_401_MentionsReauth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer server.Close()

	orig := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = orig }()
	copilotTokenExchangeURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	_, err := auth.ExchangeToken(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "invalid or expired") {
		t.Errorf("error should mention re-auth, got: %v", err)
	}
}

func TestCopilotAuth_ExchangeToken_403_MentionsSubscription(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer server.Close()

	orig := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = orig }()
	copilotTokenExchangeURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	_, err := auth.ExchangeToken(context.Background(), "valid-token-no-sub")
	if err == nil {
		t.Fatal("expected error for 403")
	}
	if !strings.Contains(err.Error(), "subscription") {
		t.Errorf("error should mention subscription, got: %v", err)
	}
}

func TestCopilotAuth_ExchangeToken_500(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer server.Close()

	orig := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = orig }()
	copilotTokenExchangeURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	_, err := auth.ExchangeToken(context.Background(), "token")
	if err == nil {
		t.Fatal("expected error for 500")
	}
}

func TestCopilotAuth_ExchangeToken_EmptyTokenReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CopilotSessionToken{Token: "", ExpiresAt: time.Now().Add(30 * time.Minute).Unix()})
	}))
	defer server.Close()

	orig := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = orig }()
	copilotTokenExchangeURL = server.URL

	auth := &CopilotAuth{client: server.Client()}
	_, err := auth.ExchangeToken(context.Background(), "token")
	if err == nil {
		t.Fatal("expected error for empty token in response")
	}
	if !strings.Contains(err.Error(), "empty token") {
		t.Errorf("error should mention empty token, got: %v", err)
	}
}

// --- GetSessionToken: caching behaviour ---

func TestCopilotAuth_GetSessionToken_CachesValidToken(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CopilotSessionToken{
			Token:     "cached-session-tok",
			ExpiresAt: time.Now().Add(2 * time.Hour).Unix(), // far future — well beyond 5-min buffer
		})
	}))
	defer server.Close()

	orig := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = orig }()
	copilotTokenExchangeURL = server.URL

	auth := &CopilotAuth{client: server.Client()}

	tok1, err := auth.GetSessionToken(context.Background(), "gh-tok")
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	tok2, err := auth.GetSessionToken(context.Background(), "gh-tok")
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	if tok1 != tok2 {
		t.Error("expected same token on second call (should be cached)")
	}
	if callCount != 1 {
		t.Errorf("expected 1 exchange call (token cached), got %d", callCount)
	}
}

func TestCopilotAuth_GetSessionToken_RefreshesNearExpiryToken(t *testing.T) {
	// Tokens returned within the 5-min pre-expiry window should trigger a refresh.
	responses := []*CopilotSessionToken{
		{Token: "tok-1", ExpiresAt: time.Now().Add(2 * time.Minute).Unix()}, // < 5 min → will refresh
		{Token: "tok-2", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()},   // far future
	}
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses[callCount])
		callCount++
	}))
	defer server.Close()

	orig := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = orig }()
	copilotTokenExchangeURL = server.URL

	auth := &CopilotAuth{client: server.Client()}

	tok1, err := auth.GetSessionToken(context.Background(), "gh-tok")
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	tok2, err := auth.GetSessionToken(context.Background(), "gh-tok")
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	if tok1 == tok2 {
		t.Error("near-expiry token should have been refreshed — expected different tokens")
	}
	if callCount != 2 {
		t.Errorf("expected 2 exchange calls (refresh on near-expiry), got %d", callCount)
	}
}

func TestCopilotAuth_GetSessionToken_PropagatesExchangeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403) // no subscription
	}))
	defer server.Close()

	orig := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = orig }()
	copilotTokenExchangeURL = server.URL

	auth := NewCopilotAuth()
	auth.client = server.Client()

	_, err := auth.GetSessionToken(context.Background(), "valid-token")
	if err == nil {
		t.Fatal("expected error when exchange fails (403)")
	}
}

func TestCopilotAuth_GetSessionToken_FreshOnFirstCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CopilotSessionToken{
			Token:     "fresh-tok",
			ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
		})
	}))
	defer server.Close()

	orig := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = orig }()
	copilotTokenExchangeURL = server.URL

	auth := NewCopilotAuth()
	auth.client = server.Client()

	tok, err := auth.GetSessionToken(context.Background(), "gh-tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "fresh-tok" {
		t.Errorf("token = %q, want fresh-tok", tok)
	}
}

func TestCopilotAuth_GetSessionToken_Concurrency(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate some work
		time.Sleep(50 * time.Millisecond)
		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CopilotSessionToken{
			Token:     "concurrent-tok",
			ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
		})
	}))
	defer server.Close()

	orig := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = orig }()
	copilotTokenExchangeURL = server.URL

	auth := &CopilotAuth{client: server.Client()}

	const numGoroutines = 10
	errChan := make(chan error, numGoroutines)
	tokChan := make(chan string, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			tok, err := auth.GetSessionToken(context.Background(), "gh-tok")
			if err != nil {
				errChan <- err
				return
			}
			tokChan <- tok
			errChan <- nil
		}()
	}

	for i := 0; i < numGoroutines; i++ {
		if err := <-errChan; err != nil {
			t.Errorf("goroutine failed: %v", err)
		}
	}

	if callCount != 1 {
		t.Errorf("expected exactly 1 exchange call, got %d", callCount)
	}

	for i := 0; i < numGoroutines; i++ {
		if tok := <-tokChan; tok != "concurrent-tok" {
			t.Errorf("wrong token: %q", tok)
		}
	}
}
