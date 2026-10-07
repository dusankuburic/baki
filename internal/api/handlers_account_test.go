package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/service"
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

// TestAccountDelete_AnnouncesErasure: the erasure must announce AccountErased
// (the provider service drops its cached listings on it — the SQL cascade
// removes the provider keys, the event tells the in-process caches) with the
// caller's key scope, and only after a successful delete.
func TestAccountDelete_AnnouncesErasure(t *testing.T) {
	rt, backend := newAccountTestRouter(t, "u1", "alice@example.com")
	bearer := jwtBearer(t, rt, "u1", "alice@example.com")
	bus := eventbus.New(nil, 0)
	rec := eventbus.NewRecorder(bus)
	moduleOf[*AuthHandler](t, rt).accounts = service.NewAccountLifecycleService(backend, nil, bus)

	// A refused confirmation announces nothing.
	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/account", bearer, map[string]string{"confirmEmail": "other@example.com"})
	checkStatus(t, rr, http.StatusBadRequest)
	if got := rec.Events(); len(got) != 0 {
		t.Errorf("announced %v on a refused confirmation", got)
	}

	rr = doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/account", bearer, map[string]string{"confirmEmail": "alice@example.com"})
	checkStatus(t, rr, http.StatusOK)
	if got := eventbus.Recorded[events.AccountErased](rec); len(got) != 1 || got[0].KeyScope != "u1" || got[0].UserID != "u1" {
		t.Errorf("AccountErased = %v, want exactly one with key scope u1 (the caller's)", got)
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
