package testutil

import (
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// NewTestFlow returns a FlowDocument with sensible defaults for tests.
// Override any field after construction.
func NewTestFlow(id, name string) *interfaces.FlowDocument {
	now := time.Now().UTC()
	return &interfaces.FlowDocument{
		ID:        id,
		Name:      name,
		Content:   []byte(`{"subflows":[]}`),
		OwnerID:   "test-user",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// NewTestUser returns a User with sensible defaults for tests.
func NewTestUser(id, email string, role auth.Role) *interfaces.User {
	now := time.Now().UTC()
	return &interfaces.User{
		ID:        id,
		Email:     email,
		Role:      role,
		CreatedAt: now,
		UpdatedAt: now,
	}
}
