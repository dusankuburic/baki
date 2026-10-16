package interfaces

import (
	"context"
	"time"
)

// AuditStore covers the append-only security/governance audit log.
type AuditStore interface {
	SaveAuditEvent(ctx context.Context, event *AuditEvent) error
	// SaveAuditEvents writes a batch all or nothing (one round trip on Postgres).
	SaveAuditEvents(ctx context.Context, events []*AuditEvent) error
	ListAuditEvents(ctx context.Context, filter AuditFilter) ([]*AuditEvent, error)
}

// AuditEvent records a user action; the on-disk spill queue persists these as JSON, so field names are a storage contract.
type AuditEvent struct {
	ID           string
	UserID       string
	Email        string
	Action       string
	ResourceType string
	ResourceID   string
	IP           string
	Meta         map[string]string
	CreatedAt    time.Time
	// Columns of v33, empty on older rows (stored as NULL). OrgID is the organisation the action belongs to;
	// ActorKind is auth.ActorKind's name ("user", "token", "system"), with TokenID for a token and ActorJob for a
	// background job; UserAgent is the request's, bounded.
	OrgID     string
	ActorKind string
	TokenID   string
	ActorJob  string
	UserAgent string
}

// AuditFilter controls which audit events are returned.
type AuditFilter struct {
	UserID string
	Action string
	Limit  int
	Offset int
	Since  *time.Time
	// IncludeSubject also matches events whose subject is UserID (sign-ins carry no actor); the GDPR export must not set it — a user's export must never carry a failed sign-in's IP.
	IncludeSubject bool
	// After is a keyset cursor (the CreatedAt and ID of the last event of the page before): only the events after it
	// in the log's order (created_at desc, id asc) match, and Offset is ignored.
	After *PageCursor
}
