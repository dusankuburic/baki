package padcloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAuthenticator_DeviceFlowAgainstMockMSAL drives the full device-code flow
// against a mock MSAL endpoint: start → poll (pending) → poll (token) → cached.
// Proves the request shape (client_id/scope/grant_type), the pending→success
// transition, and that AccessToken serves the cached token afterward.
func TestAuthenticator_DeviceFlowAgainstMockMSAL(t *testing.T) {
	var deviceCodeSeen, grantTypeSeen, scopeSeen string
	pollCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/devicecode":
			deviceCodeSeen = r.FormValue("client_id")
			scopeSeen = r.FormValue("scope")
			_ = json.NewEncoder(w).Encode(DeviceAuthResponse{
				DeviceCode: "DC-123", UserCode: "AB-CD", VerificationURI: "https://microsoft.com/devicelogin",
			})
		case "/token":
			pollCount++
			grantTypeSeen = r.FormValue("grant_type")
			if pollCount == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "eyJ-tok", "expires_in": 3600, "token_type": "Bearer",
			})
		}
	}))
	defer srv.Close()

	// Point the authenticator at the mock server.
	a := NewAuthenticator("test-tenant", "client-xyz", "https://api.powerplatform.com/.default", srv.Client(), nil)
	a.deviceCodeURL = srv.URL + "/devicecode"
	a.tokenURL = srv.URL + "/token"

	dar, err := a.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if dar.DeviceCode != "DC-123" || deviceCodeSeen != "client-xyz" || scopeSeen != "https://api.powerplatform.com/.default" {
		t.Errorf("device-code request/resp mismatch: seen client=%q scope=%q resp=%+v", deviceCodeSeen, scopeSeen, dar)
	}

	// First poll → pending.
	r1, err := a.PollToken(context.Background(), dar.DeviceCode)
	if err != nil || r1.Status != "pending" {
		t.Fatalf("first poll: status=%q err=%v, want pending", r1.Status, err)
	}
	if !strings.Contains(grantTypeSeen, "device_code") {
		t.Errorf("grant_type = %q, want the device_code grant", grantTypeSeen)
	}

	// Second poll → success + cached.
	r2, err := a.PollToken(context.Background(), dar.DeviceCode)
	if err != nil || r2.Status != "success" || r2.AccessToken != "eyJ-tok" {
		t.Fatalf("second poll: status=%q tok=%q err=%v, want success/eyJ-tok", r2.Status, r2.AccessToken, err)
	}
	if got := a.AccessToken(); got != "eyJ-tok" {
		t.Errorf("cached AccessToken = %q, want eyJ-tok", got)
	}
}
