package api

import (
	"context"
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/filesystem"
)

// TestChangePassword_Flow locks the change-password contract: the handler decodes
// the JSON key `oldPassword`. A frontend that sent `currentPassword` left it empty
// and every attempt failed with 401 — these tests would have caught that.
func TestChangePassword_Flow(t *testing.T) {
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	rt := newTestRouter(fs, true)
	ctx := context.Background()
	const old, fresh = "OldPassw0rd!", "BrandNewPassw0rd!"
	seedUserWithPassword(t, fs, "u1", "u1@example.com", old)
	bearer := jwtBearer(t, rt, "u1", "u1@example.com")

	// Correct current password under the `oldPassword` key → 200, hash updated.
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/change-password", bearer,
		map[string]string{"oldPassword": old, "newPassword": fresh})
	if rr.Code != http.StatusOK {
		t.Fatalf("change-password status = %d, body=%s", rr.Code, rr.Body.String())
	}
	u, err := fs.LoadUserByID(ctx, "u1")
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if !auth.CheckPasswordHash(fresh, u.Password) {
		t.Error("stored password hash was not updated to the new password")
	}
	if auth.CheckPasswordHash(old, u.Password) {
		t.Error("old password still validates after change")
	}
}

func TestChangePassword_WrongOldPassword(t *testing.T) {
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	rt := newTestRouter(fs, true)
	seedUserWithPassword(t, fs, "u2", "u2@example.com", "OldPassw0rd!")
	bearer := jwtBearer(t, rt, "u2", "u2@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/change-password", bearer,
		map[string]string{"oldPassword": "WrongCurrent1!", "newPassword": "BrandNewPassw0rd!"})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong old password status = %d, want 401", rr.Code)
	}
}
