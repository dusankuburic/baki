package service

import (
	"context"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// ChannelService owns an organisation's notification channels (webhook, Teams, Slack). Kind and
// URL validation, URL redaction and test deliveries stay with the caller; the store encrypts the
// channel secret at rest.
type ChannelService struct {
	store storageif.OrgChannelStore
}

func NewChannelService(store storageif.OrgChannelStore) *ChannelService {
	return &ChannelService{store: store}
}

// List returns all of the org's channels, enabled or not.
func (s *ChannelService) List(ctx context.Context, orgID string) ([]*storageif.OrgChannel, error) {
	return s.store.ListOrgChannels(ctx, orgID, false)
}

// Find returns the org's channel with id, or nil when the org has none by that id.
func (s *ChannelService) Find(ctx context.Context, orgID, id string) (*storageif.OrgChannel, error) {
	channels, err := s.store.ListOrgChannels(ctx, orgID, false)
	if err != nil {
		return nil, err
	}
	for _, c := range channels {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, nil
}

// Save upserts ch (keyed by ID).
func (s *ChannelService) Save(ctx context.Context, ch *storageif.OrgChannel) error {
	return s.store.SaveOrgChannel(ctx, ch)
}

func (s *ChannelService) Delete(ctx context.Context, orgID, id string) error {
	return s.store.DeleteOrgChannel(ctx, orgID, id)
}
