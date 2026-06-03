package api

import (
	"context"
	"time"
)

// RefreshTokenStore tracks issued refresh tokens so they can be rotated and
// revoked (cloud mode).
type RefreshTokenStore interface {
	StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time) error
	IsRefreshTokenValid(ctx context.Context, jti string) (bool, error)
	RevokeRefreshToken(ctx context.Context, jti string) error
	RevokeUserRefreshTokens(ctx context.Context, userID string) error
}
