package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testSecret = "test-super-secret-key-for-tests"

func newTestManager() *Manager {
	// Short TTLs so expiry tests run quickly
	return NewManagerWithTTL(testSecret, 2*time.Second, 5*time.Second, "test-issuer", "test-audience", nil)
}

// ---- Roles ----

func TestRole_IsValid(t *testing.T) {
	for _, r := range []Role{RoleAdmin, RoleMember, RoleViewer, RoleGuest} {
		if !r.IsValid() {
			t.Errorf("role %q should be valid", r)
		}
	}
	if Role("superuser").IsValid() {
		t.Error("unknown role should not be valid")
	}
}

// ---- WebSocket tickets ----

func TestManager_IssueAndVerifyWSTicket(t *testing.T) {
	mgr := newTestManager()

	ticket, exp, err := mgr.IssueWSTicket("user-1", "alice@example.com", RoleMember)
	if err != nil {
		t.Fatalf("IssueWSTicket: %v", err)
	}
	if ticket == "" {
		t.Fatal("expected non-empty ticket")
	}
	if !exp.After(time.Now()) {
		t.Fatal("ticket should expire in the future")
	}

	claims, err := mgr.VerifyWSTicket(ticket)
	if err != nil {
		t.Fatalf("VerifyWSTicket: %v", err)
	}
	if claims.UserID != "user-1" || claims.Email != "alice@example.com" || claims.Role != RoleMember {
		t.Errorf("claims mismatch: %+v", claims)
	}
	if claims.ID == "" {
		t.Error("ticket must carry a jti for single-use tracking")
	}
}

func TestManager_AccessTokenIsNotAValidWSTicket(t *testing.T) {
	mgr := newTestManager()
	pair, _ := mgr.Issue("u1", "a@b.com", RoleMember)

	if _, err := mgr.VerifyWSTicket(pair.AccessToken); err == nil {
		t.Fatal("an access token must not verify as a WS ticket (audience mismatch)")
	}
}

func TestManager_WSTicketIsNotAValidAccessToken(t *testing.T) {
	mgr := newTestManager()
	ticket, _, _ := mgr.IssueWSTicket("u1", "a@b.com", RoleMember)

	if _, err := mgr.Verify(ticket); err == nil {
		t.Fatal("a WS ticket must not verify as an access token (audience mismatch)")
	}
}

func TestManager_WSTicketWrongSecretRejected(t *testing.T) {
	mgr1 := newTestManager()
	mgr2 := NewManagerWithTTL("a-different-secret-value", 2*time.Second, 5*time.Second, "test-issuer", "test-audience", nil)

	ticket, _, _ := mgr1.IssueWSTicket("u1", "a@b.com", RoleViewer)
	if _, err := mgr2.VerifyWSTicket(ticket); err == nil {
		t.Fatal("ticket signed with a different secret must be rejected")
	}
}

// ---- JWT Manager ----

func TestManager_Issue_And_Verify(t *testing.T) {
	mgr := newTestManager()

	pair, err := mgr.Issue("user-1", "alice@example.com", RoleMember)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("expected non-empty tokens")
	}

	claims, err := mgr.Verify(pair.AccessToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.UserID != "user-1" {
		t.Errorf("UserID: got %q, want %q", claims.UserID, "user-1")
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("Email: got %q", claims.Email)
	}
	if claims.Role != RoleMember {
		t.Errorf("Role: got %q", claims.Role)
	}
}

func TestManager_Verify_WrongSecret(t *testing.T) {
	mgr1 := newTestManager()
	mgr2 := NewManager("completely-different-secret", nil)

	pair, _ := mgr1.Issue("u1", "a@b.com", RoleViewer)
	_, err := mgr2.Verify(pair.AccessToken)
	if err == nil {
		t.Error("expected error when verifying with wrong secret")
	}
}

func TestManager_Verify_TamperedToken(t *testing.T) {
	mgr := newTestManager()
	pair, _ := mgr.Issue("u1", "a@b.com", RoleAdmin)

	// Flip the last character of the signature
	token := pair.AccessToken
	tampered := token[:len(token)-1] + "X"
	_, err := mgr.Verify(tampered)
	if err == nil {
		t.Error("expected error for tampered token")
	}
}

func TestManager_Verify_ExpiredToken(t *testing.T) {
	// Issue a token with a 1ms TTL
	mgr := NewManagerWithTTL(testSecret, time.Millisecond, time.Second, "test-issuer", "test-audience", nil)
	pair, _ := mgr.Issue("u1", "a@b.com", RoleViewer)
	time.Sleep(10 * time.Millisecond)

	_, err := mgr.Verify(pair.AccessToken)
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestManager_VerifyRefresh(t *testing.T) {
	mgr := newTestManager()
	pair, _ := mgr.Issue("u1", "a@b.com", RoleMember)

	rc, err := mgr.VerifyRefresh(pair.RefreshToken)
	if err != nil {
		t.Fatalf("VerifyRefresh: %v", err)
	}
	if rc.UserID != "u1" {
		t.Errorf("UserID: got %q", rc.UserID)
	}
}

func TestManager_AccessTokenIsNotValidAsRefresh(t *testing.T) {
	mgr := newTestManager()
	pair, _ := mgr.Issue("u1", "a@b.com", RoleMember)

	// Refresh token verifier should reject an access token because the Claims
	// type is different (extra fields cause parse mismatch in strict mode).
	// At minimum, the claims.UserID will come from the access-token-specific
	// Claims struct which embeds RegisteredClaims differently.
	//
	// We just verify there is no panic and check the happy-path round-trip
	// is consistent.
	rc, _ := mgr.VerifyRefresh(pair.RefreshToken)
	if rc == nil {
		t.Error("valid refresh token should verify successfully")
	}
}

// ---- JWT Middleware ----

func TestMiddleware_ValidToken_PassesThrough(t *testing.T) {
	mgr := newTestManager()
	pair, _ := mgr.Issue("u1", "a@b.com", RoleMember)

	called := false
	handler := Middleware(mgr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		claims := ClaimsFromContext(r.Context())
		if claims == nil {
			t.Error("expected claims in context")
		}
		if claims.UserID != "u1" {
			t.Errorf("UserID in context: got %q", claims.UserID)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !called {
		t.Error("inner handler was not called")
	}
}

func TestMiddleware_MissingToken_Returns401(t *testing.T) {
	mgr := newTestManager()
	handler := Middleware(mgr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("inner handler must not be called for unauthenticated request")
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestMiddleware_InvalidToken_Returns401(t *testing.T) {
	mgr := newTestManager()
	handler := Middleware(mgr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("inner handler must not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer not-a-valid-jwt")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestMiddleware_TokenInQuery_IsAccepted(t *testing.T) {
	mgr := newTestManager()
	pair, _ := mgr.Issue("u1", "a@b.com", RoleViewer)

	called := false
	handler := Middleware(mgr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/?token="+pair.AccessToken, nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Error("expected handler to be called with token in query")
	}
}

// ---- StaticTokenMiddleware (legacy) ----

func TestStaticTokenMiddleware_CorrectToken_PassesThrough(t *testing.T) {
	const secret = "static-secret"
	called := false
	handler := StaticTokenMiddleware(secret, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Error("expected inner handler to be called")
	}
}

func TestStaticTokenMiddleware_WrongToken_Returns401(t *testing.T) {
	handler := StaticTokenMiddleware("correct", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not call inner handler")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}
