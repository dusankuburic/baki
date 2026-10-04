package interfaces

import (
	"context"

	"pad-analyzer/internal/auth"
)

// Optional point-lookup capabilities, deliberately NOT part of StorageBackend:
// callers type-assert and fall back to the broader method when a backend does
// not provide one. Each exists because the broader method sat on a hot path
// needing one row — an org-membership check per flow authz, the library's org
// scope per page, PAT authentication per machine request.

// OrgMembership is the caller's own membership in one organisation, without
// the organisation's roster.
type OrgMembership struct {
	OrgID   string    `json:"orgId"`
	OrgName string    `json:"orgName"`
	Role    auth.Role `json:"role"`
}

// OrgMembershipReader answers "what is userID's role in orgID" with a single
// indexed lookup instead of loading the whole organisation and roster.
type OrgMembershipReader interface {
	// OrgMemberRole returns the user's role in the organisation. member is
	// false when the organisation exists but the user is not in it; a missing
	// organisation is ErrNotFound.
	OrgMemberRole(ctx context.Context, orgID, userID string) (role auth.Role, member bool, err error)
}

// OrgMembershipLister lists the organisations a user belongs to, with the
// user's role in each, without attaching any roster.
type OrgMembershipLister interface {
	ListOrgMembershipsForUser(ctx context.Context, userID string) ([]OrgMembership, error)
}

// FindingStatusGetter reads the triage state of one finding. It returns
// (nil, nil) when the finding has no persisted state (implicitly "open").
type FindingStatusGetter interface {
	GetFindingStatus(ctx context.Context, flowID, findingKey string) (*FindingStatus, error)
}

// APITokenOwnerLookup resolves a personal access token and its owning user in
// one round trip; ErrNotFound when the token or its owner is gone.
type APITokenOwnerLookup interface {
	GetAPITokenWithOwnerByHash(ctx context.Context, tokenHash string) (*APIToken, *User, error)
}
