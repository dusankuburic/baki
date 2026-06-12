package api

import (
	"context"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// RefreshTokenStore tracks issued refresh tokens so they can be rotated and
// revoked (cloud mode).
type RefreshTokenStore interface {
	StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time) error
	IsRefreshTokenValid(ctx context.Context, jti string) (bool, error)
	RevokeRefreshToken(ctx context.Context, jti string) error
	RevokeUserRefreshTokens(ctx context.Context, userID string) error
	// ListUserRefreshTokens returns active sessions for a user, for display in
	// profile/session-management UIs.
	ListUserRefreshTokens(ctx context.Context, userID string) ([]interfaces.RefreshTokenInfo, error)
	// RevokeRefreshTokenForUser revokes a single session, scoped to userID so a
	// user cannot revoke another user's session.
	RevokeRefreshTokenForUser(ctx context.Context, jti, userID string) error
}
