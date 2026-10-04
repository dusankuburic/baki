package ai

import (
	"context"
	"net/http"
	"testing"
)

// F-51: Claude on Foundry in entra_cli mode must authorize with the Azure CLI
// token (Bearer), exactly like the service-principal Entra mode — before the
// fix the auth switch had no case for it and the request went out bare.
func TestClaudeOnFoundry_EntraCLIModeSendsBearer(t *testing.T) {
	cfg := AzureFoundryConfig{
		Endpoint: "https://my-res.openai.azure.com",
		Auth:     AzureFoundryAuth{Mode: AzureAuthModeEntraCLI},
	}
	p := newClaudeOnFoundry(cfg, nil, func(context.Context) (string, error) { return "cli-token", nil }, nil)
	if p.authorize == nil {
		t.Fatal("entra_cli mode left the Claude route without an authorizer")
	}
	req, _ := http.NewRequest(http.MethodPost, p.messagesURL, nil)
	if err := p.authorize(context.Background(), req); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer cli-token" {
		t.Errorf("Authorization = %q, want the CLI bearer token", got)
	}
	if p.unauthorized == nil {
		t.Error("entra_cli mode has no 401 handler (a rejected token would be reported as a bad key)")
	}
}
