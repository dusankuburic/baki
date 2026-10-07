package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// shareLinkDays is a new share link's lifetime, so a stale link does not grant permanent access.
const shareLinkDays = 30

// ShareTokenService owns the public, read-only report links of a flow: the store keeps only the
// token's SHA-256, the raw token is shown once at creation. Authorization is the caller's; resolving
// a token is what authorizes the unauthenticated /api/shared routes. Creating or revoking a link is announced as a
// FlowChanged of kind FlowSharingChanged.
type ShareTokenService struct {
	store storageif.ShareTokenStore
	flows storageif.FlowStore
	bus   *eventbus.Bus
}

// NewShareTokenService builds the service; a nil bus announces nothing.
func NewShareTokenService(store storageif.ShareTokenStore, flows storageif.FlowStore, bus *eventbus.Bus) *ShareTokenService {
	return &ShareTokenService{store: store, flows: flows, bus: bus}
}

// sharingChanged announces a change of the flow's links, with the flow id only (the routes do not hand over the
// flow's header, and reading it here would add a statement).
func (s *ShareTokenService) sharingChanged(ctx context.Context, flowID string) {
	s.bus.Publish(ctx, events.FlowChanged{FlowID: flowID, StableID: flowID, Kind: events.FlowSharingChanged})
}

// Create mints a link for flowID that expires after shareLinkDays and returns it with the raw token.
func (s *ShareTokenService) Create(ctx context.Context, flowID, createdBy string) (*storageif.ShareToken, string, error) {
	rawBytes := make([]byte, 32) // 64 hex chars
	if _, err := rand.Read(rawBytes); err != nil {
		return nil, "", err
	}
	rawToken := hex.EncodeToString(rawBytes)
	expiry := time.Now().AddDate(0, 0, shareLinkDays)
	t := &storageif.ShareToken{
		FlowID:    flowID,
		TokenHash: hashShareToken(rawToken),
		CreatedBy: createdBy,
		ExpiresAt: &expiry,
	}
	if err := s.store.CreateShareToken(ctx, t); err != nil {
		return nil, "", err
	}
	s.sharingChanged(ctx, flowID)
	return t, rawToken, nil
}

// List returns the flow's links (metadata only).
func (s *ShareTokenService) List(ctx context.Context, flowID string) ([]*storageif.ShareToken, error) {
	return s.store.ListShareTokens(ctx, flowID)
}

// Revoke deletes one of the flow's links.
func (s *ShareTokenService) Revoke(ctx context.Context, flowID, tokenID string) error {
	if err := s.store.RevokeShareToken(ctx, flowID, tokenID); err != nil {
		return err
	}
	s.sharingChanged(ctx, flowID)
	return nil
}

// Resolve looks up a raw token; an unknown or expired one is an error.
func (s *ShareTokenService) Resolve(ctx context.Context, rawToken string) (*storageif.ShareToken, error) {
	return s.store.GetShareTokenByHash(ctx, hashShareToken(rawToken))
}

// SharedFlowHeader resolves a raw token to its flow's header (no content), nil when the token or
// the flow is unknown.
func (s *ShareTokenService) SharedFlowHeader(ctx context.Context, rawToken string) *storageif.FlowDocument {
	st, err := s.Resolve(ctx, rawToken)
	if err != nil || st == nil {
		return nil
	}
	doc, err := s.flows.LoadFlowHeader(ctx, st.FlowID)
	if err != nil || doc == nil {
		return nil
	}
	return doc
}

func hashShareToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}
