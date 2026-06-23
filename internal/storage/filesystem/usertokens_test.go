package filesystem

import (
	"context"
	"errors"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

func TestUserTokens_ConsumeOnce(t *testing.T) {
	b, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	ctx := context.Background()
	tok := &interfaces.UserToken{
		TokenHash: "hash-1",
		Purpose:   interfaces.TokenPurposePasswordReset,
		UserID:    "user-1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := b.CreateUserToken(ctx, tok); err != nil {
		t.Fatalf("CreateUserToken: %v", err)
	}

	uid, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposePasswordReset, "hash-1")
	if err != nil || uid != "user-1" {
		t.Fatalf("first consume: uid=%q err=%v", uid, err)
	}
	// Single-use: a second consume must fail.
	if _, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposePasswordReset, "hash-1"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("replay should be ErrNotFound, got %v", err)
	}
}

func TestUserTokens_PurposeAndExpiry(t *testing.T) {
	b, _ := NewLocalStorageBackend(t.TempDir())
	ctx := context.Background()

	_ = b.CreateUserToken(ctx, &interfaces.UserToken{
		TokenHash: "h-purpose", Purpose: interfaces.TokenPurposePasswordReset,
		UserID: "u", ExpiresAt: time.Now().Add(time.Hour),
	})
	// Wrong purpose must not match.
	if _, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposeEmailVerify, "h-purpose"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("purpose mismatch should be ErrNotFound, got %v", err)
	}

	// Expired token must not match.
	_ = b.CreateUserToken(ctx, &interfaces.UserToken{
		TokenHash: "h-exp", Purpose: interfaces.TokenPurposeEmailVerify,
		UserID: "u", ExpiresAt: time.Now().Add(-time.Minute),
	})
	if _, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposeEmailVerify, "h-exp"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("expired token should be ErrNotFound, got %v", err)
	}
}

func TestSetUserEmailVerified(t *testing.T) {
	b, _ := NewLocalStorageBackend(t.TempDir())
	ctx := context.Background()
	u := &interfaces.User{ID: "u-1", Email: "u@x.com"}
	if err := b.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := b.SetUserEmailVerified(ctx, "u-1"); err != nil {
		t.Fatalf("SetUserEmailVerified: %v", err)
	}
	got, err := b.LoadUserByID(ctx, "u-1")
	if err != nil {
		t.Fatalf("LoadUserByID: %v", err)
	}
	if !got.EmailVerified {
		t.Error("expected EmailVerified=true after SetUserEmailVerified")
	}
}
