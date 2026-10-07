package service

import (
	"context"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// APITokenService manages a user's machine API tokens (personal access tokens). Only the hash is ever stored.
type APITokenService struct {
	store storageif.APITokenStore
}

func NewAPITokenService(store storageif.APITokenStore) *APITokenService {
	return &APITokenService{store: store}
}

// Create stores a new token record (its hash, never the raw token).
func (s *APITokenService) Create(ctx context.Context, tok *storageif.APIToken) error {
	return s.store.CreateAPIToken(ctx, tok)
}

// List returns userID's tokens (metadata only); never nil.
func (s *APITokenService) List(ctx context.Context, userID string) ([]*storageif.APIToken, error) {
	tokens, err := s.store.ListAPITokens(ctx, userID)
	if err != nil {
		return nil, err
	}
	if tokens == nil {
		tokens = []*storageif.APIToken{}
	}
	return tokens, nil
}

// Delete revokes userID's token id; idempotent, and one user cannot revoke another's.
func (s *APITokenService) Delete(ctx context.Context, userID, id string) error {
	return s.store.DeleteAPIToken(ctx, userID, id)
}
