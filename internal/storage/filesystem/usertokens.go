package filesystem

import (
	"context"
	"fmt"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// One-shot user tokens (password reset, email verification) for the
// filesystem/test backend. Held in memory like API tokens — desktop mode has no
// auth, so these flows are exercised only by tests, where in-memory is enough.

func (lsb *LocalStorageBackend) CreateUserToken(ctx context.Context, t *interfaces.UserToken) error {
	if t == nil || t.TokenHash == "" || t.Purpose == "" {
		return fmt.Errorf("user token requires tokenHash and purpose")
	}
	lsb.userTokenMu.Lock()
	defer lsb.userTokenMu.Unlock()

	if lsb.userTokens == nil {
		lsb.userTokens = make(map[string]*interfaces.UserToken)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	cp := *t
	lsb.userTokens[t.TokenHash] = &cp
	return nil
}

func (lsb *LocalStorageBackend) ConsumeUserToken(ctx context.Context, purpose, tokenHash string) (string, error) {
	lsb.userTokenMu.Lock()
	defer lsb.userTokenMu.Unlock()

	t, ok := lsb.userTokens[tokenHash]
	if !ok || t.Purpose != purpose || time.Now().UTC().After(t.ExpiresAt) {
		return "", interfaces.ErrNotFound
	}
	// Single-use: delete on redemption so it cannot be replayed.
	delete(lsb.userTokens, tokenHash)
	return t.UserID, nil
}

func (lsb *LocalStorageBackend) SetUserEmailVerified(ctx context.Context, userID string) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()

	u, ok := lsb.users[userID]
	if !ok {
		return interfaces.ErrNotFound
	}
	u.EmailVerified = true
	return nil
}
