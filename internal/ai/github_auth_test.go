package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Both device flows poll GitHub's token endpoint the same way; only the client id differs.
func devicePollers(client *http.Client) map[string]func(context.Context, string) (*GitHubAuthResult, error) {
	return map[string]func(context.Context, string) (*GitHubAuthResult, error){
		"github":  (&GitHubAuth{client: client}).PollToken,
		"copilot": (&CopilotAuth{client: client}).PollToken,
	}
}

func TestGitHubAuth_PollToken_Success_UsesCorrectClientID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("client_id"); got != githubClientID {
			t.Errorf("client_id = %q, want %q", got, githubClientID)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"access_token": "gho_models123"})
	}))
	defer server.Close()

	orig := githubTokenURL
	defer func() { githubTokenURL = orig }()
	githubTokenURL = server.URL

	auth := &GitHubAuth{client: server.Client()}
	result, err := auth.PollToken(context.Background(), "dev-code")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "success" || result.Token != "gho_models123" {
		t.Errorf("result = %+v, want status success with the granted token", result)
	}
}

// A 200 that carries neither an access token nor an error is outside GitHub's device-flow contract. Relayed as
// success it would make the client switch to a provider with nothing stored: the service saves only a non-empty
// token, and the login button stops on status error but keeps polling through a 5xx.
func TestPollToken_NoTokenIsNotSuccess(t *testing.T) {
	for _, body := range []string{`{}`, `{"access_token":""}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		}))
		orig := githubTokenURL
		githubTokenURL = server.URL

		for name, poll := range devicePollers(server.Client()) {
			t.Run(name+" "+body, func(t *testing.T) {
				result, err := poll(context.Background(), "dev-code")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result.Status != "error" || result.Token != "" || result.Error == "" {
					t.Errorf("result = %+v, want status error with a message and no token", result)
				}
			})
		}

		githubTokenURL = orig
		server.Close()
	}
}
