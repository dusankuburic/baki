package api

import (
	"context"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// RefreshTokenStore tracks issued refresh tokens so they can be rotated and
// revoked (cloud mode).
type RefreshTokenStore interface {
	// userAgent and ip are captured from the issuing request so the session
	// can later be shown with a friendly device label.
	StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time, userAgent, ip string) error
	IsRefreshTokenValid(ctx context.Context, jti string) (bool, error)
	RevokeRefreshToken(ctx context.Context, jti string) error
	// VerifyAndRevokeRefreshToken atomically verifies and revokes a refresh token
	// in a single atomic operation. Returns the token info if successful,
	// or ErrTokenAlreadyRevoked if the token was already revoked/invalid.
	// This eliminates the race window between VerifyRefresh and RevokeRefreshToken.
	VerifyAndRevokeRefreshToken(ctx context.Context, jti string) (*interfaces.RefreshTokenInfo, error)
	RevokeUserRefreshTokens(ctx context.Context, userID string) error
	// ListUserRefreshTokens returns active sessions for a user, for display in
	// profile/session-management UIs.
	ListUserRefreshTokens(ctx context.Context, userID string) ([]interfaces.RefreshTokenInfo, error)
	// RevokeRefreshTokenForUser revokes a single session, scoped to userID so a
	// user cannot revoke another user's session.
	RevokeRefreshTokenForUser(ctx context.Context, jti, userID string) error
}
