package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// newAccountTestRouter builds a JWT-enabled router backed by a FakeBackend with
// the given user seeded, returning the backend so the test can assert erasure.
func newAccountTestRouter(t *testing.T, userID, email string) (*Router, *testutil.FakeBackend) {
	t.Helper()
	backend := &testutil.FakeBackend{Users: map[string]*storageif.User{
		userID: {ID: userID, Email: email, Role: auth.RoleMember},
	}}
	return newTestRouter(backend, true), backend
}

func TestAccountDelete_RequiresMatchingConfirmation(t *testing.T) {
	rt, _ := newAccountTestRouter(t, "u1", "alice@example.com")
	bearer := jwtBearer(t, rt, "u1", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/account", bearer, map[string]string{})
	checkStatus(t, rr, http.StatusBadRequest)

	rr = doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/account", bearer, map[string]string{"confirmEmail": "other@example.com"})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestAccountDelete_ErasesAndRevokesToken(t *testing.T) {
	rt, backend := newAccountTestRouter(t, "u1", "alice@example.com")
	bearer := jwtBearer(t, rt, "u1", "alice@example.com")

	// Matching confirmation (case-insensitive, trimmed) → 200.
	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/account", bearer, map[string]string{"confirmEmail": "  ALICE@example.com "})
	checkStatus(t, rr, http.StatusOK)

	if len(backend.DeletedUsers) != 1 || backend.DeletedUsers[0] != "u1" {
		t.Errorf("expected user u1 erased, got %v", backend.DeletedUsers)
	}

	// The caller's access token was revoked: a follow-up authenticated request
	// must now be rejected (401).
	rr2 := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/me", bearer, nil)
	checkStatus(t, rr2, http.StatusUnauthorized)
}

// TestAccountDelete_RunsUserDeletedHook: the erasure must run the
// onUserDeleted hook (DI wires it to ProviderService.InvalidateScope — the
// SQL cascade removes the provider keys, the hook tells the in-process
// caches) with the caller's key scope, and only after a successful delete.
func TestAccountDelete_RunsUserDeletedHook(t *testing.T) {
	rt, _ := newAccountTestRouter(t, "u1", "alice@example.com")
	bearer := jwtBearer(t, rt, "u1", "alice@example.com")

	var mu sync.Mutex
	var scopes []string
	moduleOf[*AuthHandler](t, rt).accounts.OnUserDeleted(func(scope string) {
		mu.Lock()
		scopes = append(scopes, scope)
		mu.Unlock()
	})

	// A refused confirmation must not fire the hook.
	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/account", bearer, map[string]string{"confirmEmail": "other@example.com"})
	checkStatus(t, rr, http.StatusBadRequest)
	mu.Lock()
	fired := len(scopes)
	mu.Unlock()
	if fired != 0 {
		t.Errorf("hook fired on a refused confirmation: %v", scopes)
	}

	rr = doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/account", bearer, map[string]string{"confirmEmail": "alice@example.com"})
	checkStatus(t, rr, http.StatusOK)

	mu.Lock()
	defer mu.Unlock()
	if len(scopes) != 1 || scopes[0] != "u1" {
		t.Errorf("onUserDeleted scopes = %v, want exactly [u1] (the caller's key scope)", scopes)
	}
}

func TestAccountExport_ReturnsUserData(t *testing.T) {
	rt, _ := newAccountTestRouter(t, "u1", "alice@example.com")
	bearer := jwtBearer(t, rt, "u1", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/account/export", bearer, nil)
	checkStatus(t, rr, http.StatusOK)

	if got := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("expected attachment Content-Disposition, got %q", got)
	}
	var export storageif.UserDataExport
	if err := json.Unmarshal(rr.Body.Bytes(), &export); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if export.User == nil || export.User.ID != "u1" || export.User.Email != "alice@example.com" {
		t.Errorf("unexpected export user: %+v", export.User)
	}
	if export.ExportedAt.IsZero() {
		t.Error("expected ExportedAt to be set")
	}
}
