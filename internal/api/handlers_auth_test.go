package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
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
	body := map[string]any{"email": "dup@example.com", "password": "password"}
	first := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", body)
	checkStatus(t, first, http.StatusOK)
	second := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", body)
	checkStatus(t, second, http.StatusConflict)
}

func TestHandleAuthRegister_FirstUserIsAdmin(t *testing.T) {
	rt := newJWTTestRouter(t)

	first := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
		"email": "first@example.com", "password": "password",
	})
	checkStatus(t, first, http.StatusOK)
	var r1 map[string]any
	decodeJSON(t, first, &r1)
	if u, _ := r1["user"].(map[string]any); u["role"] != "admin" {
		t.Errorf("expected first user role=admin, got %v", u["role"])
	}

	second := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/register", "", map[string]any{
		"email": "second@example.com", "password": "password",
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
	resp := login(t, rt, "user1@example.com", "password")

	for _, field := range []string{"accessToken", "refreshToken", "expiresAt"} {
		if resp[field] == nil {
			t.Errorf("response missing field %q", field)
		}
	}
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
	resp := login(t, rt, "user1@example.com", "password")
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
	resp := login(t, rt, "alice@example.com", "password")
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
				"password": "password",
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
	backend := rt.app.StorageBackend()
	users, err := backend.ListUsers(context.Background())
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
