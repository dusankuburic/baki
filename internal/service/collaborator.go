package service

import (
	"context"
	"errors"
	"strings"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// ErrAlreadyCollaborator is returned by CollaboratorService.Add when the backend refuses the grant as a duplicate.
var ErrAlreadyCollaborator = errors.New("user is already a collaborator")

// collaboratorStore is the storage the collaborator routes use: the flow's grants and the two user lookups that
// resolve whom a grant is for.
type collaboratorStore interface {
	ListCollaborators(ctx context.Context, flowID string, limit int) ([]*storageif.Collaborator, error)
	AddCollaborator(ctx context.Context, flowID string, c *storageif.Collaborator) error
	UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error
	RemoveCollaborator(ctx context.Context, flowID, userID string) error
	LoadUserByID(ctx context.Context, id string) (*storageif.User, error)
	LoadUserByEmail(ctx context.Context, email string) (*storageif.User, error)
}

// CollaboratorService manages who a flow is shared with. Authorization stays with the caller (the route's
// requireFlow, or the handler's own check for self-removal); nothing is cached, so a change invalidates nothing.
// The store is nil on the desktop, whose routes never reach it (route.Needs or the handler's own nil check).
type CollaboratorService struct {
	store collaboratorStore
}

func NewCollaboratorService(store collaboratorStore) *CollaboratorService {
	return &CollaboratorService{store: store}
}

// List returns up to limit grants on the flow (limit <= 0 = all).
func (s *CollaboratorService) List(ctx context.Context, flowID string, limit int) ([]*storageif.Collaborator, error) {
	return s.store.ListCollaborators(ctx, flowID, limit)
}

// ResolveUser finds the user a grant is for: by id when one is given, else by email.
func (s *CollaboratorService) ResolveUser(ctx context.Context, userID, email string) (*storageif.User, error) {
	if userID != "" {
		return s.store.LoadUserByID(ctx, userID)
	}
	return s.store.LoadUserByEmail(ctx, email)
}

// Add grants u the permission on the flow and returns the stored grant; ErrAlreadyCollaborator when the backend
// reports a duplicate.
func (s *CollaboratorService) Add(ctx context.Context, flowID string, u *storageif.User, permission string) (*storageif.Collaborator, error) {
	c := &storageif.Collaborator{
		UserID:     u.ID,
		Email:      u.Email,
		Permission: permission,
		GrantedAt:  time.Now().UTC(),
	}
	if err := s.store.AddCollaborator(ctx, flowID, c); err != nil {
		if msg := strings.ToLower(err.Error()); strings.Contains(msg, "duplicate") || strings.Contains(msg, "already") {
			return nil, ErrAlreadyCollaborator
		}
		return nil, err
	}
	return c, nil
}

// Update changes an existing grant's permission; storage's ErrNotFound when there is none.
func (s *CollaboratorService) Update(ctx context.Context, flowID, userID, permission string) error {
	return s.store.UpdateCollaborator(ctx, flowID, userID, permission)
}

// Remove revokes a grant; storage's ErrNotFound when there is none.
func (s *CollaboratorService) Remove(ctx context.Context, flowID, userID string) error {
	return s.store.RemoveCollaborator(ctx, flowID, userID)
}
