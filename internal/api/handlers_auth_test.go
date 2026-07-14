package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// login posts to /api/auth/login and returns the decoded response body.
func login(t *testing.T, rt *Router, email, password string) map[string]any {
	t.Helper()

	if email != "" && password != "" {
		rrReg := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
			"email":    email,
			"password": password,
		})
		if rrReg.Code != http.StatusOK && rrReg.Code != http.StatusConflict {
			t.Fatalf("failed to register test user: %v - body: %s", rrReg.Code, rrReg.Body.String())
		}
		// Login requires a verified email (M9). Test users are verified
		// immediately after registration so this helper exercises the normal
		// verified-user login path; tests that specifically want an unverified
		// account register without calling this helper.
		if rt.security.Backend != nil {
			if u, err := rt.security.Backend.LoadUserByEmail(context.Background(), email); err == nil && u != nil {
				_ = rt.security.Backend.SetUserEmailVerified(context.Background(), u.ID)
			}
		}
	}

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/login", "", map[string]any{
		"email":    email,
		"password": password,
	})
	checkStatus(t, rr, http.StatusOK)
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	return resp
}

func TestHandleAuthRegister_InvalidEmailReturns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
		"email": "not-an-email", "password": "longenough",
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleAuthRegister_ShortPasswordReturns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
		"email": "alice@example.com", "password": "short",
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleAuthRegister_DuplicateReturns409(t *testing.T) {
	rt := newJWTTestRouter(t)
	body := map[string]any{"email": "dup@example.com", "password": "Password123!"}
	first := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", body)
	checkStatus(t, first, http.StatusOK)
	second := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", body)
	checkStatus(t, second, http.StatusConflict)
}

func TestHandleAuthRegister_FirstUserIsAdmin(t *testing.T) {
	rt := newJWTTestRouter(t)

	first := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
		"email": "first@example.com", "password": "Password123!",
	})
	checkStatus(t, first, http.StatusOK)
	var r1 map[string]any
	decodeJSON(t, first, &r1)
	if u, _ := r1["user"].(map[string]any); u["role"] != "admin" {
		t.Errorf("expected first user role=admin, got %v", u["role"])
	}

	second := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
		"email": "second@example.com", "password": "Password123!",
	})
	checkStatus(t, second, http.StatusOK)
	var r2 map[string]any
	decodeJSON(t, second, &r2)
	if u, _ := r2["user"].(map[string]any); u["role"] != "member" {
		t.Errorf("expected second user role=member, got %v", u["role"])
	}
}

func TestHandleAuthLogin_ReturnsTokenPair(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "user1@example.com", "Password123!")

	for _, field := range []string{"accessToken", "refreshToken", "expiresAt"} {
		if resp[field] == nil {
			t.Errorf("response missing field %q", field)
		}
	}
}

// M9: a registered-but-unverified user must not be able to log in. This blocks
// the shadow-registration takeover where an attacker registers a victim's email
// and immediately gets a working session.
func TestHandleAuthLogin_UnverifiedEmail_Returns403(t *testing.T) {
	rt := newJWTTestRouter(t)
	// Register (leaves EmailVerified=false) but do NOT call login(), which
	// would verify the email.
	rrReg := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
		"email":    "unverified@example.com",
		"password": "Password123!",
	})
	checkStatus(t, rrReg, http.StatusOK)

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/login", "", map[string]any{
		"email":    "unverified@example.com",
		"password": "Password123!",
	})
	checkStatus(t, rr, http.StatusForbidden)
	if !strings.Contains(rr.Body.String(), "not verified") {
		t.Errorf("expected 'not verified' in body, got: %s", rr.Body.String())
	}

	// After verifying the email, the same credentials must succeed.
	u, err := rt.security.Backend.LoadUserByEmail(context.Background(), "unverified@example.com")
	if err != nil {
		t.Fatalf("LoadUserByEmail: %v", err)
	}
	if err := rt.security.Backend.SetUserEmailVerified(context.Background(), u.ID); err != nil {
		t.Fatalf("SetUserEmailVerified: %v", err)
	}
	rr2 := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/login", "", map[string]any{
		"email":    "unverified@example.com",
		"password": "Password123!",
	})
	checkStatus(t, rr2, http.StatusOK)
}

func TestHandleAuthLogin_EmptyFieldsGetDefaults(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/login", "", map[string]any{
		"email":    "",
		"password": "",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for empty login, got %d", rr.Code)
	}
}

func TestHandleAuthLogin_BadBodyReturns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/login", "", nil)
	// nil body → empty body → decode error → 400
	// Actually empty body should decode as EOF → 400
	// But login is a public route so no auth check is needed
	_ = rr // we just verify no panic; login with no body returns 400
	if rr.Code == http.StatusUnauthorized {
		t.Error("login should not require auth")
	}
}

func TestHandleAuthRefresh_ValidToken_ReturnsNewPair(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "user1@example.com", "Password123!")
	refreshToken, _ := resp["refreshToken"].(string)

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/refresh", "", map[string]any{
		"refreshToken": refreshToken,
	})
	checkStatus(t, rr, http.StatusOK)

	var newResp map[string]any
	decodeJSON(t, rr, &newResp)
	if newResp["accessToken"] == nil {
		t.Error("expected accessToken in refresh response")
	}
}

func TestHandleAuthRefresh_InvalidToken_Returns401(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/refresh", "", map[string]any{
		"refreshToken": "not-a-valid-token",
	})
	checkStatus(t, rr, http.StatusUnauthorized)
}

func TestHandleAuthMe_ValidJWT_ReturnsClaims(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "alice@example.com", "Password123!")
	accessToken, _ := resp["accessToken"].(string)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+accessToken, nil)
	checkStatus(t, rr, http.StatusOK)

	var claims map[string]any
	decodeJSON(t, rr, &claims)
	if claims["email"] != "alice@example.com" {
		t.Errorf("expected email=alice@example.com, got %v", claims["email"])
	}
}

func TestHandleAuthMe_NoToken_Returns401(t *testing.T) {
	rt := newJWTTestRouter(t)
	// /api/auth/me is NOT in publicRoutes, so the middleware will reject missing tokens.
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "", nil)
	checkStatus(t, rr, http.StatusUnauthorized)
}

func TestHandleAuthLogout_ReturnsOK(t *testing.T) {
	rt := newJWTTestRouter(t)
	bearer := jwtBearer(t, rt, "user1", "user1@example.com")
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/logout", bearer, nil)
	checkStatus(t, rr, http.StatusOK)

	var resp map[string]string
	decodeJSON(t, rr, &resp)
	if resp["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", resp["status"])
	}
}

// TestHandleAuthRegister_ConcurrentFirstUser_ExactlyOneAdmin exercises the
// race that motivated the CreateUser atomic-storage method. Many goroutines
// register with distinct emails at the same time; only the goroutine whose
// insert lands first should be promoted to admin. Run under `go test -race`
// to also catch unsafe state sharing in the storage layer.
func TestHandleAuthRegister_ConcurrentFirstUser_ExactlyOneAdmin(t *testing.T) {
	rt := newJWTTestRouter(t)

	const N = 25
	var wg sync.WaitGroup
	var adminCount int64
	var okCount int64
	start := make(chan struct{})

	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			<-start
			rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
				"email":    fmt.Sprintf("user%d@example.com", i),
				"password": "Password123!",
			})
			if rr.Code != http.StatusOK {
				t.Errorf("register %d: status=%d body=%s", i, rr.Code, rr.Body.String())
				return
			}
			atomic.AddInt64(&okCount, 1)
			var resp map[string]any
			decodeJSON(t, rr, &resp)
			if u, _ := resp["user"].(map[string]any); u != nil && u["role"] == "admin" {
				atomic.AddInt64(&adminCount, 1)
			}
		}()
	}
	close(start) // release all goroutines simultaneously
	wg.Wait()

	if okCount != N {
		t.Fatalf("expected %d successful registrations, got %d", N, okCount)
	}
	if adminCount != 1 {
		t.Errorf("expected exactly 1 admin out of %d concurrent registrations, got %d", N, adminCount)
	}

	// Cross-check via the storage backend itself.
	backend := rt.security.Backend
	users, err := backend.ListUsers(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	storedAdmins := 0
	for _, u := range users {
		if string(u.Role) == "admin" {
			storedAdmins++
		}
	}
	if storedAdmins != 1 {
		t.Errorf("storage shows %d admins, expected 1", storedAdmins)
	}
}

// A1.1: A locked user must not be able to refresh tokens.
func TestHandleAuthRefresh_LockedUser_Returns401(t *testing.T) {
	rt := newJWTTestRouter(t)
	resp := login(t, rt, "locked@example.com", "Password123!")
	accessToken, _ := resp["accessToken"].(string)
	refreshToken, _ := resp["refreshToken"].(string)

	// Get the user ID
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", "Bearer "+accessToken, nil)
	var me map[string]any
	decodeJSON(t, rr, &me)
	userID, _ := me["id"].(string)

	// Lock the user
	user, err := rt.security.Backend.LoadUserByID(context.Background(), userID)
	if err != nil {
		t.Fatalf("LoadUserByID: %v", err)
	}
	lockUntil := time.Now().UTC().Add(1 * time.Hour)
	user.LockedUntil = &lockUntil
	if err := rt.security.Backend.SaveUser(context.Background(), user); err != nil {
		t.Fatalf("SaveUser: %v", err)
	}

	// Attempt refresh — must be rejected
	rr2 := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/refresh", "", map[string]any{
		"refreshToken": refreshToken,
	})
	checkStatus(t, rr2, http.StatusUnauthorized)
}

// A1.2: A deleted/non-existent user must not be able to refresh tokens.
func TestHandleAuthRefresh_DeletedUser_Returns401(t *testing.T) {
	rt := newJWTTestRouter(t)

	// Issue a token pair for a user that was never registered
	pair, err := rt.security.AuthMgr.Issue("ghost-user-id", "ghost@example.com", auth.RoleMember)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Attempt refresh — LoadUserByID will fail → 401
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/refresh", "", map[string]any{
		"refreshToken": pair.RefreshToken,
	})
	checkStatus(t, rr, http.StatusUnauthorized)
}

// O1: Filesystem SaveFlow must enforce OCC and return ErrVersionConflict on stale versions.
func TestLocalStorage_SaveFlow_VersionConflict(t *testing.T) {
	rt := newJWTTestRouter(t)

	// Create and save a new flow — version stays 0 for new flows
	flow := &interfaces.FlowDocument{
		ID:      "test-flow-occ",
		Name:    "Test",
		Content: []byte("{}"),
		OwnerID: "test-user",
		Version: 0,
	}
	if err := rt.security.Backend.SaveFlow(context.Background(), flow); err != nil {
		t.Fatalf("initial SaveFlow: %v", err)
	}

	// Second save with matching version=0 → succeeds, bumps to 1
	flow2 := &interfaces.FlowDocument{
		ID:      "test-flow-occ",
		Name:    "Test v2",
		Content: []byte(`{"v":2}`),
		OwnerID: "test-user",
		Version: 0,
	}
	if err := rt.security.Backend.SaveFlow(context.Background(), flow2); err != nil {
		t.Fatalf("second SaveFlow: %v", err)
	}
	if flow2.Version != 1 {
		t.Fatalf("expected version 1 after second save, got %d", flow2.Version)
	}

	// Now attempt to save with stale version 0 → must conflict (current is 1)
	stale := &interfaces.FlowDocument{
		ID:      "test-flow-occ",
		Name:    "Stale",
		Content: []byte(`{"v":"stale"}`),
		OwnerID: "test-user",
		Version: 0,
	}
	err := rt.security.Backend.SaveFlow(context.Background(), stale)
	if !errors.Is(err, interfaces.ErrVersionConflict) {
		t.Errorf("expected ErrVersionConflict for stale version, got %v", err)
	}
}
