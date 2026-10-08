package interfaces

import (
	"context"
	"time"
)

// SharingStore covers per-flow collaborator management.
type SharingStore interface {
	// ListCollaborators returns up to limit rows (limit <= 0 = all).
	ListCollaborators(ctx context.Context, flowID string, limit int) ([]*Collaborator, error)
	ListCollaboratorsBatch(ctx context.Context, flowIDs []string) (map[string][]*Collaborator, error)
	AddCollaborator(ctx context.Context, flowID string, c *Collaborator) error
	UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error
	RemoveCollaborator(ctx context.Context, flowID, userID string) error
}

// Collaborator represents a user with access to a specific flow.
type Collaborator struct {
	UserID     string    `json:"userId"`
	Email      string    `json:"email"`
	Permission string    `json:"permission"`
	GrantedAt  time.Time `json:"grantedAt"`
}
