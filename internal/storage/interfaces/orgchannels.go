package interfaces

import (
	"context"
	"time"
)

// OrgChannel is one org-configured notification destination for the org's governance events; URL kinds: webhook / teams / slack.
type OrgChannel struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"orgId"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	Secret    string    `json:"-"` // HMAC key; never serialized to clients
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// OrgChannelStore covers per-org notification channel management.
type OrgChannelStore interface {
	// SaveOrgChannel upserts a channel (ID-keyed); Kind/URL validation is the API's concern.
	SaveOrgChannel(ctx context.Context, ch *OrgChannel) error
	DeleteOrgChannel(ctx context.Context, orgID, id string) error
	// ListOrgChannels returns the org's channels; enabledOnly for dispatch.
	ListOrgChannels(ctx context.Context, orgID string, enabledOnly bool) ([]*OrgChannel, error)
}
