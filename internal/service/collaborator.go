package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
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
// requireFlow, or the handler's own check for self-removal); each change is announced as a FlowChanged of kind
// FlowSharingChanged. The store is nil on the desktop, whose routes never reach it (route.Needs or the handler's
// own nil check).
type CollaboratorService struct {
	store collaboratorStore
	bus   *eventbus.Bus
}

// NewCollaboratorService builds the service; a nil bus announces nothing.
func NewCollaboratorService(store collaboratorStore, bus *eventbus.Bus) *CollaboratorService {
	return &CollaboratorService{store: store, bus: bus}
}

// sharingChanged announces a change of the flow's grants. Only the flow id: the routes decide on the flow without
// handing its header over, and reading it here would add a statement.
func (s *CollaboratorService) sharingChanged(ctx context.Context, flowID string) {
	s.bus.Publish(ctx, events.FlowChanged{FlowID: flowID, StableID: flowID, Kind: events.FlowSharingChanged})
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
	s.sharingChanged(ctx, flowID)
	return c, nil
}

// Update changes an existing grant's permission; storage's ErrNotFound when there is none.
func (s *CollaboratorService) Update(ctx context.Context, flowID, userID, permission string) error {
	if err := s.store.UpdateCollaborator(ctx, flowID, userID, permission); err != nil {
		return err
	}
	s.sharingChanged(ctx, flowID)
	return nil
}

// Remove revokes a grant; storage's ErrNotFound when there is none.
func (s *CollaboratorService) Remove(ctx context.Context, flowID, userID string) error {
	if err := s.store.RemoveCollaborator(ctx, flowID, userID); err != nil {
		return err
	}
	s.sharingChanged(ctx, flowID)
	return nil
}
