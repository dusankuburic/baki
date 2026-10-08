package interfaces

import (
	"context"
	"time"
)

// OrgInviteStore keeps the invitations to join an organisation (OrgInvite). An invite goes with its organisation;
// account erasure deletes the invitations to the user's email and clears the user as an inviter; the retention sweep
// removes expired invites and those accepted more than seven days ago.
type OrgInviteStore interface {
	// SaveOrgInvite inserts the invite, stamping a zero CreatedAt (on invite). An id already stored, or a second
	// pending invite for the same organisation and email, is ErrOrgInviteExists; the organisation must exist.
	SaveOrgInvite(ctx context.Context, invite *OrgInvite) error
	// ListOrgInvites returns the organisation's invites, pending and resolved, newest first.
	ListOrgInvites(ctx context.Context, orgID string) ([]*OrgInvite, error)
	// GetOrgInvite returns the organisation's invite; ErrNotFound when it has none with that id.
	GetOrgInvite(ctx context.Context, orgID, inviteID string) (*OrgInvite, error)
	// GetOrgInviteByTokenHash returns the invite whose token hashes to tokenHash, accepted or not; ErrNotFound.
	GetOrgInviteByTokenHash(ctx context.Context, tokenHash string) (*OrgInvite, error)
	// DeleteOrgInvite deletes the organisation's invite; ErrNotFound when it has none with that id.
	DeleteOrgInvite(ctx context.Context, orgID, inviteID string) error
	// MarkOrgInviteAccepted stamps acceptedAt on a pending invite, once: an accepted or missing invite is
	// ErrOrgInviteAlreadyAccepted, so two concurrent acceptances cannot both succeed.
	MarkOrgInviteAccepted(ctx context.Context, inviteID string, acceptedAt time.Time) error
}
