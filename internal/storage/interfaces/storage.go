package interfaces

import (
	"context"
	"errors"
)

// ErrNotFound is returned by backend methods when the requested record does not exist.
var ErrNotFound = errors.New("not found")

// ErrRLSUnavailable is returned by every statement of a request whose RLS transaction could not be started: a mutation must not fall through to the pool, where RLS short-circuits to allow-all (HTTP 503).
var ErrRLSUnavailable = errors.New("rls: scoped transaction unavailable")

// HealthChecker covers backend liveness/lifecycle; every backend must support it regardless of role.
type HealthChecker interface {
	Ping(ctx context.Context) error
	// CurrentSchemaVersion is the highest schema migration the database has applied (0 for a backend without one).
	CurrentSchemaVersion(ctx context.Context) (int, error)
	Close() error
}

// StorageBackend is the full storage capability set composed from the role interfaces; depend on the narrowest role interface(s) you use.
type StorageBackend interface {
	FlowStore
	OrgChannelStore
	CICredentialStore
	OrgCustomRuleStore
	FlowVersionStore
	FlowSnapshotStore
	AppSettingsStore
	ConversationStore
	UserStore
	OrgStore
	OrgInviteStore
	SharingStore
	UsageStore
	DashboardStore
	KnowledgeStore
	HealthChecker
	AuditStore
	TriageStore
	CommentStore
	ShareTokenStore
	APITokenStore
	UserTokenStore
	PolicyStore
	GovernanceAlertStore
	RuleNoticeStore
	RuleSetStore
	RefreshTokenStore
	IdentityLinkStore
	JobRunStore
}
