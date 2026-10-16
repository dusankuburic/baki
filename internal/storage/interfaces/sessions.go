package interfaces

import (
	"context"
	"errors"
	"time"
)

// ErrTokenAlreadyRevoked is returned by RevokeRefreshToken when the token was already revoked — treat as replay and revoke all of the user's sessions.
var ErrTokenAlreadyRevoked = errors.New("refresh token already revoked")

// ErrAccountVerified is SaveUserWithIdentityLink refusing to adopt an account verified since it was read.
var ErrAccountVerified = errors.New("the account is already verified")

// IdentityLink ties an external IdP identity (provider + stable subject ID) to a local user account for OIDC SSO.
type IdentityLink struct {
	Provider  string    `json:"provider"`
	Subject   string    `json:"subject"`
	UserID    string    `json:"userId"`
	Email     string    `json:"email,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// RefreshTokenInfo describes a single issued refresh token, surfaced to users as an "active session".
type RefreshTokenInfo struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// UserAgent and IP identify the device/location so the user can recognize and revoke unfamiliar sessions; may be empty on old tokens.
	UserAgent string `json:"userAgent,omitempty"`
	IP        string `json:"ip,omitempty"`
}

// RefreshTokenStore tracks issued refresh tokens (the user's sessions) so they can be rotated and revoked.
type RefreshTokenStore interface {
	// StoreRefreshToken records an issued token; a jti already stored is left as it is. userAgent and ip label the session for its owner.
	StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time, userAgent, ip string) error
	// IsRefreshTokenValid reports whether jti is stored, unrevoked and unexpired.
	IsRefreshTokenValid(ctx context.Context, jti string) (bool, error)
	// RevokeRefreshToken revokes one token; ErrTokenAlreadyRevoked when it is unknown or already revoked (a rotation race).
	RevokeRefreshToken(ctx context.Context, jti string) error
	// VerifyAndRevokeRefreshToken atomically revokes a valid, unexpired token and returns it; ErrTokenAlreadyRevoked otherwise (replay).
	VerifyAndRevokeRefreshToken(ctx context.Context, jti string) (*RefreshTokenInfo, error)
	RevokeUserRefreshTokens(ctx context.Context, userID string) error
	// ListUserRefreshTokens returns userID's active sessions, newest first.
	ListUserRefreshTokens(ctx context.Context, userID string) ([]RefreshTokenInfo, error)
	// RevokeRefreshTokenForUser revokes one of userID's own active sessions (a user cannot revoke another's); ErrNotFound when userID has no such session.
	RevokeRefreshTokenForUser(ctx context.Context, jti, userID string) error
}

// IdentityLinkStore maps external (SSO) identities to local users; one external identity maps to exactly one user.
type IdentityLinkStore interface {
	// SaveIdentityLink stores a new link; an identity already linked (to anyone) is an error, never re-pointed.
	SaveIdentityLink(ctx context.Context, link *IdentityLink) error
	// SaveUserWithIdentityLink stores user and link as SaveIdentityLink does, in one transaction: an SSO sign-up never
	// leaves an account without the identity that signs it in. A new id is inserted. An existing one is an adoption:
	// only its email_verified and password take user's values, and only while the stored row is unverified (the
	// rest of user may be stale); a row verified meanwhile is ErrAccountVerified. A taken email is ErrEmailExists
	// and an identity already linked an error; in every refusal nothing is written.
	SaveUserWithIdentityLink(ctx context.Context, user *User, link *IdentityLink) error
	// LoadIdentityLink returns the link of an external identity, or ErrNotFound.
	LoadIdentityLink(ctx context.Context, provider, subject string) (*IdentityLink, error)
	// ListIdentityLinks returns userID's links, oldest first; never nil.
	ListIdentityLinks(ctx context.Context, userID string) ([]*IdentityLink, error)
	// DeleteIdentityLink removes userID's link for provider; absent is a no-op.
	DeleteIdentityLink(ctx context.Context, userID, provider string) error
}
