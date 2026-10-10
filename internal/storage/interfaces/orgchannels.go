package interfaces

import (
	"context"
	"time"
)

// OrgChannel is one org-configured notification destination for the org's governance events: a notify kind of
// organisation scope (webhook, teams, teams-workflow, slack, jira, email). Its url and secret fields are the URL and
// Secret columns; the kind's other fields are Config (public) and Secrets (sealed at rest as one value).
type OrgChannel struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"orgId"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	Secret    string    `json:"-"` // HMAC key; never serialized to clients
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	// Config holds the kind's public fields beyond url (jira's email and project, an email channel's recipients).
	Config map[string]string `json:"-"`
	// Secrets holds the kind's secret fields beyond secret (jira's API token); never serialized to clients.
	Secrets map[string]string `json:"-"`
	// Events are the event types the channel receives; nil is the default set (notify.DefaultOrgEvents).
	Events []string `json:"-"`
	// Unreadable is set by ListOrgChannels on a row whose sealed values no configured key opens (the encryption key
	// changed): the row is listed with URL, Secret and Secrets empty instead of failing the whole list. Never stored.
	Unreadable bool `json:"-"`
}

// OrgChannelStore covers per-org notification channel management.
type OrgChannelStore interface {
	// SaveOrgChannel upserts a channel (ID-keyed, scoped to its org); an update keeps the row's created_at. Kind and
	// URL validation and an edit's keep-on-omit are service.ChannelService's.
	SaveOrgChannel(ctx context.Context, ch *OrgChannel) error
	DeleteOrgChannel(ctx context.Context, orgID, id string) error
	// ListOrgChannels returns the org's channels; enabledOnly for dispatch. A row whose sealed values cannot be opened
	// is listed with Unreadable set, never an error for the whole list.
	ListOrgChannels(ctx context.Context, orgID string, enabledOnly bool) ([]*OrgChannel, error)
}
