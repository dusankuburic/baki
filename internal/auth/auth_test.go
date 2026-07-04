package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

	ticket, exp, err := mgr.IssueWSTicket("user-1", "alice@example.com", RoleMember, "src-jti-1", time.Now().Add(time.Minute))
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
	// The source access-token JTI/expiry must round-trip so the WebSocket
	// handler can re-check the access token's revocation (logout) and enforce
	// its expiry on the live socket.
	if claims.SrcJTI != "src-jti-1" {
		t.Errorf("SrcJTI round-trip: got %q, want %q", claims.SrcJTI, "src-jti-1")
	}
	if claims.SrcExp == nil || claims.SrcExp.IsZero() {
		t.Error("SrcExp must be populated when an access expiry is provided")
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
	ticket, _, _ := mgr.IssueWSTicket("u1", "a@b.com", RoleMember, "", time.Time{})

	if _, err := mgr.Verify(ticket); err == nil {
		t.Fatal("a WS ticket must not verify as an access token (audience mismatch)")
	}
}

func TestManager_WSTicketWrongSecretRejected(t *testing.T) {
	mgr1 := newTestManager()
	mgr2 := NewManagerWithTTL("a-different-secret-value", 2*time.Second, 5*time.Second, "test-issuer", "test-audience", nil)

	ticket, _, _ := mgr1.IssueWSTicket("u1", "a@b.com", RoleViewer, "", time.Time{})
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

	// Tamper with a character in the middle of the signature (not the last
	// char — base64url trailing bits can be padding-only and flipping them
	// may not change the decoded signature, causing intermittent false passes).
	parts := strings.Split(pair.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
	sig := parts[2]
	if len(sig) < 4 {
		t.Fatal("signature too short to tamper")
	}
	mid := len(sig) / 2
	replacement := byte('Z')
	if sig[mid] == 'Z' {
		replacement = 'Y'
	}
	tamperedSig := sig[:mid] + string(replacement) + sig[mid+1:]
	tampered := parts[0] + "." + parts[1] + "." + tamperedSig

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

	// The happy-path refresh token verifies.
	rc, err := mgr.VerifyRefresh(pair.RefreshToken)
	if err != nil || rc == nil {
		t.Fatalf("valid refresh token should verify successfully: %v", err)
	}

	// An access token must NOT verify as a refresh token: access and refresh
	// carry distinct audiences, so VerifyRefresh (which enforces the refresh
	// audience) rejects it. This stops an access token being replayed at
	// /auth/refresh.
	if _, err := mgr.VerifyRefresh(pair.AccessToken); err == nil {
		t.Error("access token must be rejected by VerifyRefresh (audience separation)")
	}
}

func TestManager_RefreshTokenIsNotValidAsAccess(t *testing.T) {
	mgr := newTestManager()
	pair, _ := mgr.Issue("u1", "a@b.com", RoleMember)

	// A refresh token must NOT verify as an access token. Without distinct
	// audiences, a leaked refresh token (long TTL) would work as a bearer
	// access token and bypass the rotation/replay checks that only run at
	// /auth/refresh.
	if _, err := mgr.Verify(pair.RefreshToken); err == nil {
		t.Error("refresh token must be rejected by Verify (audience separation)")
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
			return
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

func TestMiddleware_TokenInQuery_AcceptedOnlyOnSSEPath(t *testing.T) {
	mgr := newTestManager()
	pair, _ := mgr.Issue("u1", "a@b.com", RoleViewer)

	// On the SSE endpoint, the query fallback must still work (EventSource
	// cannot set headers), so the handler is called.
	t.Run("sse path accepts query token", func(t *testing.T) {
		called := false
		handler := Middleware(mgr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/events?token="+pair.AccessToken, nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)
		if !called {
			t.Error("expected handler to be called with token in query on /api/events")
		}
	})

	// On every other route the query fallback is refused to keep access JWTs
	// out of proxy/browser logs — the handler must NOT be called.
	t.Run("non-sse path rejects query token", func(t *testing.T) {
		called := false
		handler := Middleware(mgr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/flow/upload?token="+pair.AccessToken, nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)
		if called {
			t.Error("handler must NOT be called when token is only in query on a non-SSE path")
		}
	})
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

// TestManager_RefreshToken_BothTokensValid tests that VerifyRefresh succeeds
// for both the access and refresh tokens of a pair. The concurrent-race test
// lives in the API/storage layer (security_integration_test.go) because the
// race is at the DB level (VerifyAndRevokeRefreshToken), not in JWT verification.
func TestManager_RefreshToken_BothTokensValid(t *testing.T) {
	mgr := newTestManager()
	pair, _ := mgr.Issue("u1", "a@b.com", RoleMember)

	// Access token should verify
	_, err := mgr.Verify(pair.AccessToken)
	if err != nil {
		t.Errorf("access token verification failed: %v", err)
	}

	// Refresh token should verify
	rc, err := mgr.VerifyRefresh(pair.RefreshToken)
	if err != nil {
		t.Errorf("refresh token verification failed: %v", err)
	}
	if rc.UserID != "u1" {
		t.Errorf("expected userID u1, got %s", rc.UserID)
	}
}
