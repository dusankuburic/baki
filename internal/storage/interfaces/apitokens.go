package interfaces

import (
	"context"
	"time"
)

// APITokenStore covers machine credentials (personal access tokens).
type APITokenStore interface {
	CreateAPIToken(ctx context.Context, t *APIToken) error
	// GetAPITokenByHash resolves a token by hash for authentication, or ErrNotFound; revocation is a delete, so a revoked token is not found.
	GetAPITokenByHash(ctx context.Context, tokenHash string) (*APIToken, error)
	// GetAPITokenWithOwnerByHash resolves a token and its owner's current user row (without the password hash) in one round trip; ErrNotFound when either is gone.
	GetAPITokenWithOwnerByHash(ctx context.Context, tokenHash string) (*APIToken, *User, error)
	// ListAPITokens returns a user's tokens (metadata only; never the hash to clients).
	ListAPITokens(ctx context.Context, userID string) ([]*APIToken, error)
	// DeleteAPIToken removes a token owned by userID (one user cannot revoke another's); idempotent.
	DeleteAPIToken(ctx context.Context, userID, id string) error
}

// APIToken is a scoped, revocable machine credential authenticating API calls as its owner; only the SHA-256 hash is persisted, the raw value shown once and unrecoverable.
type APIToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	Name      string     `json:"name"`
	TokenHash string     `json:"-"` // sha256 hex; never serialized to clients
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"` // nil ⇒ no expiry
	// Scopes restricts a PAT's capabilities (empty = unscoped = full access); members of auth.ValidTokenScopes only.
	Scopes []string `json:"scopes,omitempty"`
}
