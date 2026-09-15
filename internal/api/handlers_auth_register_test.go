package api

import (
	"net/http"
	"testing"
)

// TestRegister_FirstUserIsVerifiedAndSignedIn pins the bootstrap contract: on
// an empty deployment the first registration becomes admin, is email-verified
// (nobody to shadow-register) and receives a session directly — the
// docker-compose quick start has no SMTP relay, so without this the very first
// administrator could never sign in.
func TestRegister_FirstUserIsVerifiedAndSignedIn(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
		"email":    "founder@example.com",
		"password": "StrongPass123!",
	})
	checkStatus(t, rr, http.StatusOK)
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["verificationRequired"] != false {
		t.Fatalf("first user should not need verification, got %v", resp["verificationRequired"])
	}
	if tok, _ := resp["accessToken"].(string); tok == "" {
		t.Fatalf("first user should receive a session, body: %s", rr.Body.String())
	}
	user, _ := resp["user"].(map[string]any)
	if user["role"] != "admin" {
		t.Fatalf("first user should be admin, got %v", user["role"])
	}
	if rr.Header().Get("Set-Cookie") == "" {
		t.Fatalf("expected the refresh cookie to be set on register-with-session")
	}
}

// TestRegister_SecondUserRequiresVerification verifies that every registration
// after the bootstrap admin gets NO session and is told to verify — the
// response the frontend's RegisterResponseSchema is written against.
func TestRegister_SecondUserRequiresVerification(t *testing.T) {
	rt := newJWTTestRouter(t)
	login(t, rt, "founder@example.com", "StrongPass123!") // consumes the bootstrap slot

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
		"email":    "second@example.com",
		"password": "StrongPass123!",
	})
	checkStatus(t, rr, http.StatusOK)
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["verificationRequired"] != true {
		t.Fatalf("second user must require verification, body: %s", rr.Body.String())
	}
	if _, has := resp["accessToken"]; has {
		t.Fatalf("second user must not receive a session before verifying, body: %s", rr.Body.String())
	}

	// Login before verification is refused with a machine-readable code so
	// the UI can offer a resend instead of a bare error.
	rrLogin := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/login", "", map[string]any{
		"email":    "second@example.com",
		"password": "StrongPass123!",
	})
	checkStatus(t, rrLogin, http.StatusForbidden)
	var errResp map[string]any
	decodeJSON(t, rrLogin, &errResp)
	if errResp["code"] != "email_not_verified" {
		t.Fatalf("expected code email_not_verified, body: %s", rrLogin.Body.String())
	}
}

// TestResendVerification_AlwaysOK verifies the resend endpoint responds
// identically for unverified, verified, unknown and malformed addresses — the
// anti-enumeration posture shared with forgot-password.
func TestResendVerification_AlwaysOK(t *testing.T) {
	rt := newJWTTestRouter(t)
	login(t, rt, "founder@example.com", "StrongPass123!") // verified (bootstrap)
	reg := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
		"email":    "pending@example.com",
		"password": "StrongPass123!",
	})
	checkStatus(t, reg, http.StatusOK)

	for _, email := range []string{"pending@example.com", "founder@example.com", "nobody@example.com", "not-an-email"} {
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/resend-verification", "", map[string]any{"email": email})
		checkStatus(t, rr, http.StatusOK)
		var resp map[string]string
		decodeJSON(t, rr, &resp)
		if resp["status"] != "ok" {
			t.Errorf("%s: expected status ok, body: %s", email, rr.Body.String())
		}
	}
}

// TestResendVerification_LocalModeForbidden mirrors the other pre-auth email
// endpoints: without JWT auth there is no account to verify.
func TestResendVerification_LocalModeForbidden(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/resend-verification", "", map[string]any{"email": "x@example.com"})
	checkStatus(t, rr, http.StatusForbidden)
}
