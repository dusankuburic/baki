package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
)

func seedUserWithPassword(t *testing.T, fs *filesystem.LocalStorageBackend, id, email, password string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if err := fs.CreateUser(context.Background(), &storageif.User{ID: id, Email: email, Password: hash, Role: auth.RoleMember}); err != nil {
		t.Fatalf("create user: %v", err)
	}
}

func TestResetPassword_Flow(t *testing.T) {
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	rt := newTestRouter(fs, true)
	ctx := context.Background()
	seedUserWithPassword(t, fs, "u1", "u1@example.com", "OldPassw0rd!")

	const raw = "reset-raw-token"
	if err := fs.CreateUserToken(ctx, &storageif.UserToken{
		TokenHash: auth.HashOpaqueToken(raw),
		Purpose:   storageif.TokenPurposePasswordReset,
		UserID:    "u1",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/reset-password", "",
		map[string]string{"token": raw, "newPassword": "BrandNewPassw0rd!"})
	if rr.Code != http.StatusOK {
		t.Fatalf("reset-password status = %d, body=%s", rr.Code, rr.Body.String())
	}

	u, err := fs.LoadUserByID(ctx, "u1")
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if !auth.CheckPasswordHash("BrandNewPassw0rd!", u.Password) {
		t.Error("password was not updated to the new value")
	}

	// Token is single-use: replaying it now fails.
	rr2 := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/reset-password", "",
		map[string]string{"token": raw, "newPassword": "AnotherPassw0rd!"})
	if rr2.Code != http.StatusBadRequest {
		t.Errorf("replay status = %d, want 400", rr2.Code)
	}
}

func TestResetPassword_InvalidToken(t *testing.T) {
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	rt := newTestRouter(fs, true)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/reset-password", "",
		map[string]string{"token": "nope", "newPassword": "BrandNewPassw0rd!"})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestVerifyEmail_Flow(t *testing.T) {
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	rt := newTestRouter(fs, true)
	ctx := context.Background()
	seedUserWithPassword(t, fs, "u2", "u2@example.com", "OldPassw0rd!")

	const raw = "verify-raw-token"
	if err := fs.CreateUserToken(ctx, &storageif.UserToken{
		TokenHash: auth.HashOpaqueToken(raw),
		Purpose:   storageif.TokenPurposeEmailVerify,
		UserID:    "u2",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/verify-email", "",
		map[string]string{"token": raw})
	if rr.Code != http.StatusOK {
		t.Fatalf("verify-email status = %d, body=%s", rr.Code, rr.Body.String())
	}
	u, _ := fs.LoadUserByID(ctx, "u2")
	if !u.EmailVerified {
		t.Error("expected EmailVerified=true after verify-email")
	}
}

func TestForgotPassword_NoEnumeration(t *testing.T) {
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	rt := newTestRouter(fs, true)
	seedUserWithPassword(t, fs, "u3", "known@example.com", "OldPassw0rd!")

	for _, email := range []string{"known@example.com", "unknown@example.com"} {
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/forgot-password", "",
			map[string]string{"email": email})
		if rr.Code != http.StatusOK {
			t.Errorf("forgot-password(%s) status = %d, want 200 (no enumeration)", email, rr.Code)
		}
	}
}
