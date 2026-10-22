package api

// R-provider-1: the device-flow poll routes answer a refused token save with
// a code the client stops on, never status success.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/testutil"
)

// grantingExchange stands in for GitHub's token endpoint: every poll grants a token.
type grantingExchange struct{}

func (grantingExchange) PollToken(context.Context, string) (*ai.GitHubAuthResult, error) {
	return &ai.GitHubAuthResult{Status: "success", Token: "gho_1"}, nil
}

// pollDeviceAuth posts a poll to path on a handler whose KeyStore refuses every save with saveErr.
func pollDeviceAuth(t *testing.T, path string, saveErr error) *httptest.ResponseRecorder {
	t.Helper()
	secrets := azRegFailingSecrets{FakeSecretStore: testutil.NewFakeSecretStore(), err: saveErr}
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "", nil }, nil, nil, nil)
	svc := service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, nil, nil,
		service.WithDeviceTokenExchange(grantingExchange{}, grantingExchange{}))
	h := NewProviderHandler(svc, &SecurityConfig{}, nil)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"deviceCode":"dev-1"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	if strings.Contains(path, "/github/") {
		h.handlePollGitHubAuth(rr, req)
	} else {
		h.handlePollCopilotAuth(rr, req)
	}
	return rr
}

// A granted exchange whose token the KeyStore refuses: 503 SECRET_STORAGE_UNAVAILABLE when the secret service
// is unreachable (the Azure routes' code for it), 500 DEVICE_TOKEN_NOT_STORED for any other refusal; neither
// body says status success.
func TestPollDeviceAuth_RefusedSaveAnswersCode(t *testing.T) {
	for _, path := range []string{"/api/providers/github/poll", "/api/providers/copilot/poll"} {
		t.Run(path, func(t *testing.T) {
			var body struct {
				Status string `json:"status"`
				Code   string `json:"code"`
			}

			rr := pollDeviceAuth(t, path, storage.ErrSecretStorageUnavailable)
			checkStatus(t, rr, http.StatusServiceUnavailable)
			decodeJSON(t, rr, &body)
			if body.Status == "success" || body.Code != "SECRET_STORAGE_UNAVAILABLE" {
				t.Errorf("secret storage unavailable: body = %+v, want code SECRET_STORAGE_UNAVAILABLE and no success", body)
			}

			rr = pollDeviceAuth(t, path, errors.New("disk full"))
			checkStatus(t, rr, http.StatusInternalServerError)
			decodeJSON(t, rr, &body)
			if body.Status == "success" || body.Code != "DEVICE_TOKEN_NOT_STORED" {
				t.Errorf("other save error: body = %+v, want code DEVICE_TOKEN_NOT_STORED and no success", body)
			}
		})
	}
}

// R-provider-1: the poll routes' OpenAPI entries name both refusal answers, so the contract the client branches
// on is documented. Reads the generated docs/swagger.json, which CI keeps in step with the swag annotations.
func TestPollDeviceAuth_RoutesDocumentRefusalCodes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "swagger.json"))
	if err != nil {
		t.Fatalf("read swagger.json: %v", err)
	}
	var spec struct {
		Paths map[string]struct {
			Post struct {
				Responses map[string]struct {
					Description string `json:"description"`
					Schema      struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"responses"`
			} `json:"post"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse swagger.json: %v", err)
	}
	for _, path := range []string{"/api/providers/github/poll", "/api/providers/copilot/poll"} {
		responses := spec.Paths[path].Post.Responses
		for status, code := range map[string]string{"503": "SECRET_STORAGE_UNAVAILABLE", "500": "DEVICE_TOKEN_NOT_STORED"} {
			r := responses[status]
			if !strings.Contains(r.Description, code) || r.Schema.Ref != "#/definitions/render.ErrorResponse" {
				t.Errorf("%s: response %s = %+v, want %s on a render.ErrorResponse", path, status, r, code)
			}
		}
	}
}
