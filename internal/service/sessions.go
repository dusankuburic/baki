package service

import (
	"context"
	"errors"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// RefreshTokenStore tracks issued refresh tokens so they can be rotated and revoked (cloud mode).
type RefreshTokenStore interface {
	// userAgent and ip are captured from the issuing request so the session can later be shown with a friendly
	// device label.
	StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time, userAgent, ip string) error
	IsRefreshTokenValid(ctx context.Context, jti string) (bool, error)
	RevokeRefreshToken(ctx context.Context, jti string) error
	// VerifyAndRevokeRefreshToken atomically verifies and revokes a refresh token. Returns the token info if
	// successful, or ErrTokenAlreadyRevoked if the token was already revoked/invalid.
	VerifyAndRevokeRefreshToken(ctx context.Context, jti string) (*storageif.RefreshTokenInfo, error)
	RevokeUserRefreshTokens(ctx context.Context, userID string) error
	// ListUserRefreshTokens returns active sessions for a user, for display in profile/session-management UIs.
	ListUserRefreshTokens(ctx context.Context, userID string) ([]storageif.RefreshTokenInfo, error)
	// RevokeRefreshTokenForUser revokes a single session, scoped to userID so a user cannot revoke another user's
	// session.
	RevokeRefreshTokenForUser(ctx context.Context, jti, userID string) error
}

var (
	// ErrSessionReplayed: the refresh token was already used; every session of the user has been revoked.
	ErrSessionReplayed = errors.New("refresh token replayed")
	// ErrSessionUserMismatch: the refresh token's stored owner is not the user its claims name.
	ErrSessionUserMismatch = errors.New("refresh token user mismatch")
)

// SessionService owns the refresh-token sessions (rotation, replay detection, listing, revocation). Without a
// token store (desktop, local mode) Enabled is false and the caller decides what that means.
type SessionService struct {
	store RefreshTokenStore
}

func NewSessionService(store RefreshTokenStore) *SessionService {
	return &SessionService{store: store}
}

// Enabled reports whether a token store is wired; safe on a nil service.
func (s *SessionService) Enabled() bool { return s != nil && s.store != nil }

// Record persists a newly issued refresh token.
func (s *SessionService) Record(ctx context.Context, jti, userID string, expiresAt time.Time, userAgent, ip string) error {
	return s.store.StoreRefreshToken(ctx, jti, userID, expiresAt, userAgent, ip)
}

// Rotate consumes refresh token jti presented with userID's claims. Of two concurrent rotations of the same jti
// only one succeeds; a replayed token revokes every session of the user (ErrSessionReplayed), and a token stored
// for another user is refused (ErrSessionUserMismatch).
func (s *SessionService) Rotate(ctx context.Context, userID, jti string) error {
	info, err := s.store.VerifyAndRevokeRefreshToken(ctx, jti)
	if err != nil {
		if errors.Is(err, storageif.ErrTokenAlreadyRevoked) {
			logger.FromContext(ctx).Warn("detected refresh token replay: revoking all sessions", "userID", userID, "tokenID", jti)
			if err := s.store.RevokeUserRefreshTokens(ctx, userID); err != nil {
				logger.FromContext(ctx).Error("failed to revoke all sessions after replay detection", "error", err, "userID", userID)
			}
			return ErrSessionReplayed
		}
		logger.FromContext(ctx).Error("failed to verify-and-revoke refresh token", "error", err, "tokenID", jti)
		return err
	}
	// A1.3: a forged token must not rotate another user's refresh token.
	if info.UserID != userID {
		logger.FromContext(ctx).Warn("refresh token user mismatch", "claimsUserID", userID, "dbUserID", info.UserID, "tokenID", jti)
		return ErrSessionUserMismatch
	}
	return nil
}

// Revoke revokes one refresh token.
func (s *SessionService) Revoke(ctx context.Context, jti string) error {
	return s.store.RevokeRefreshToken(ctx, jti)
}

// RevokeAll revokes every session of userID.
func (s *SessionService) RevokeAll(ctx context.Context, userID string) error {
	return s.store.RevokeUserRefreshTokens(ctx, userID)
}

// List returns userID's active sessions; never nil.
func (s *SessionService) List(ctx context.Context, userID string) ([]storageif.RefreshTokenInfo, error) {
	sessions, err := s.store.ListUserRefreshTokens(ctx, userID)
	if err != nil {
		return nil, err
	}
	if sessions == nil {
		sessions = []storageif.RefreshTokenInfo{}
	}
	return sessions, nil
}

// RevokeForUser revokes one of userID's own sessions; ErrNotFound when userID has no such session.
func (s *SessionService) RevokeForUser(ctx context.Context, jti, userID string) error {
	return s.store.RevokeRefreshTokenForUser(ctx, jti, userID)
}
