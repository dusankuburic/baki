package interfaces

import (
	"context"
	"time"
)

// ShareTokenStore covers read-only public report links.
type ShareTokenStore interface {
	CreateShareToken(ctx context.Context, t *ShareToken) error
	GetShareTokenByHash(ctx context.Context, tokenHash string) (*ShareToken, error)
	ListShareTokens(ctx context.Context, flowID string) ([]*ShareToken, error)
	RevokeShareToken(ctx context.Context, flowID, tokenID string) error
}

// ShareToken is a revocable, read-only public link to a flow's findings report; only the SHA-256 hash is persisted, the raw value shown once.
type ShareToken struct {
	ID        string     `json:"id"`
	FlowID    string     `json:"flowId"`
	TokenHash string     `json:"-"` // sha256 hex; never serialized to clients
	CreatedBy string     `json:"createdBy,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"` // nil ⇒ no expiry
}
