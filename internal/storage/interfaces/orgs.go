package interfaces

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/auth"
)

// ErrOrgInviteExists is returned when an org+email pair already has an active (unaccepted) invite (map to HTTP 409).
var ErrOrgInviteExists = errors.New("an active invite for this email already exists")

// ErrOrgNotFound is a write of an organisation's policy, channel, custom rule or knowledge document refused because the
// organisation does not exist (the foreign keys of migration 0048). It is an ErrNotFound.
var ErrOrgNotFound = fmt.Errorf("%w: no such organisation", ErrNotFound)

// ErrOrgInviteAlreadyAccepted is returned by MarkOrgInviteAccepted when the invite was already accepted or no longer exists; single-use is enforced via `WHERE accepted_at IS NULL`.
var ErrOrgInviteAlreadyAccepted = errors.New("org invite has already been accepted or no longer exists")

// OrgStore covers organisation CRUD and membership lookup.
type OrgStore interface {
	SaveOrg(ctx context.Context, org *Organisation) error
	LoadOrg(ctx context.Context, id string) (*Organisation, error)
	ListOrgsForUser(ctx context.Context, userID string) ([]*Organisation, error)
	DeleteOrg(ctx context.Context, id string) error
	// MutateOrg applies fn to the stored organisation under its row lock and saves the name, owner, UpdatedAt (as fn leaves it) and roster (members keep their joining time, as with SaveOrg); an error from fn is returned with nothing written, a missing organisation is ErrNotFound.
	MutateOrg(ctx context.Context, id string, fn func(*Organisation) error) error
	// OrgMemberRole returns userID's role in orgID without loading the roster (every flow authz check asks); member is false when the organisation exists without the user, and a missing organisation is ErrNotFound.
	OrgMemberRole(ctx context.Context, orgID, userID string) (role auth.Role, member bool, err error)
	// ListOrgMembershipsForUser lists the organisations userID belongs to with the role in each, without rosters, most recently updated first; never nil.
	ListOrgMembershipsForUser(ctx context.Context, userID string) ([]OrgMembership, error)
}

// Organisation represents a team or workspace that owns shared flows.
type Organisation struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	OwnerID   string      `json:"ownerId"`
	Members   []OrgMember `json:"members"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

// OrgMember represents a user's membership in an organisation.
type OrgMember struct {
	UserID   string    `json:"userId"`
	Role     auth.Role `json:"role"`
	JoinedAt time.Time `json:"joinedAt"`
}

// OrgMembership is the caller's own membership in one organisation, without the organisation's roster
// (OrgStore.ListOrgMembershipsForUser).
type OrgMembership struct {
	OrgID   string    `json:"orgId"`
	OrgName string    `json:"orgName"`
	Role    auth.Role `json:"role"`
}

// OrgInvite represents a pending (or resolved) invitation to join an organisation; only the SHA-256 hash of the raw token is persisted.
type OrgInvite struct {
	ID         string     `json:"id"`
	OrgID      string     `json:"orgId"`
	Email      string     `json:"email"`
	Role       auth.Role  `json:"role"`
	InvitedBy  string     `json:"invitedBy"`
	TokenHash  string     `json:"-"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}
