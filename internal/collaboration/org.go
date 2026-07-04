package collaboration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// Errors returned by the organisation service.
var (
	ErrOrgNotFound    = errors.New("collaboration: organisation not found")
	ErrMemberNotFound = errors.New("collaboration: member not found")
	ErrAlreadyMember  = errors.New("collaboration: user is already a member")
	ErrLastAdmin      = errors.New("collaboration: cannot remove the last admin")
	ErrNotOrgAdmin    = errors.New("collaboration: user is not an admin of this organisation")

	// ErrInviteNotFound is returned when an invite ID or token has no match.
	ErrInviteNotFound = errors.New("collaboration: invite not found")
	// ErrInviteExpired is returned when an invite's expiry has passed.
	ErrInviteExpired = errors.New("collaboration: invite has expired")
	// ErrInviteAlreadyAccepted is returned when an invite token has already been used.
	ErrInviteAlreadyAccepted = errors.New("collaboration: invite has already been accepted")
)

// DefaultInviteTTL is how long a newly created org invite remains valid.
const DefaultInviteTTL = 7 * 24 * time.Hour

// OrgService manages organisations and their memberships.
// Business logic lives here; persistence is delegated to an OrgStore.
type OrgService struct {
	store OrgStore
}

// NewOrgService creates an OrgService backed by the given store.
// Pass NewMemOrgStore() for local/test mode, or a postgres-backed store for cloud mode.
func NewOrgService(store OrgStore) *OrgService {
	return &OrgService{store: store}
}

// Create creates a new organisation owned by ownerID.
func (s *OrgService) Create(ctx context.Context, name, ownerID string) (*interfaces.Organisation, error) {
	if name == "" {
		return nil, errors.New("collaboration: organisation name is required")
	}
	if ownerID == "" {
		return nil, errors.New("collaboration: owner ID is required")
	}

	now := time.Now().UTC()
	org := &interfaces.Organisation{
		ID:        uuid.New().String(),
		Name:      name,
		OwnerID:   ownerID,
		Members:   []interfaces.OrgMember{{UserID: ownerID, Role: auth.RoleAdmin, JoinedAt: now}},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.SaveOrg(ctx, org); err != nil {
		return nil, err
	}
	return org, nil
}

// Get returns the organisation with the given ID.
func (s *OrgService) Get(ctx context.Context, orgID string) (*interfaces.Organisation, error) {
	return s.store.LoadOrg(ctx, orgID)
}

// ListForUser returns all organisations the user belongs to.
func (s *OrgService) ListForUser(ctx context.Context, userID string) ([]*interfaces.Organisation, error) {
	return s.store.ListOrgsForUser(ctx, userID)
}

// AddMember adds userID to orgID with the given role.
func (s *OrgService) AddMember(ctx context.Context, orgID, userID string, role auth.Role) error {
	if !role.IsValid() {
		return fmt.Errorf("collaboration: invalid role %q", role)
	}

	return s.store.MutateOrg(ctx, orgID, func(org *interfaces.Organisation) error {
		for _, m := range org.Members {
			if m.UserID == userID {
				return ErrAlreadyMember
			}
		}
		org.Members = append(org.Members, interfaces.OrgMember{
			UserID:   userID,
			Role:     role,
			JoinedAt: time.Now().UTC(),
		})
		org.UpdatedAt = time.Now().UTC()
		return nil
	})
}

// RemoveMember removes userID from orgID.
func (s *OrgService) RemoveMember(ctx context.Context, orgID, userID string) error {
	return s.store.MutateOrg(ctx, orgID, func(org *interfaces.Organisation) error {
		idx := -1
		for i, m := range org.Members {
			if m.UserID == userID {
				idx = i
				break
			}
		}
		if idx == -1 {
			return ErrMemberNotFound
		}

		if org.Members[idx].Role == auth.RoleAdmin && adminCount(org) == 1 {
			return ErrLastAdmin
		}

		org.Members = append(org.Members[:idx], org.Members[idx+1:]...)
		org.UpdatedAt = time.Now().UTC()
		return nil
	})
}

// SetRole changes a member's role within an organisation.
func (s *OrgService) SetRole(ctx context.Context, orgID, userID string, role auth.Role) error {
	if !role.IsValid() {
		return fmt.Errorf("collaboration: invalid role %q", role)
	}

	return s.store.MutateOrg(ctx, orgID, func(org *interfaces.Organisation) error {
		for i, m := range org.Members {
			if m.UserID == userID {
				if m.Role == auth.RoleAdmin && role != auth.RoleAdmin && adminCount(org) == 1 {
					return ErrLastAdmin
				}
				org.Members[i].Role = role
				org.UpdatedAt = time.Now().UTC()
				return nil
			}
		}
		return ErrMemberNotFound
	})
}

// IsMember reports whether userID is a member of orgID.
func (s *OrgService) IsMember(ctx context.Context, orgID, userID string) bool {
	org, err := s.store.LoadOrg(ctx, orgID)
	if err != nil {
		return false
	}
	for _, m := range org.Members {
		if m.UserID == userID {
			return true
		}
	}
	return false
}

// MemberRole returns the role of userID in orgID.
func (s *OrgService) MemberRole(ctx context.Context, orgID, userID string) (auth.Role, error) {
	org, err := s.store.LoadOrg(ctx, orgID)
	if err != nil {
		return "", err
	}
	for _, m := range org.Members {
		if m.UserID == userID {
			return m.Role, nil
		}
	}
	return "", ErrMemberNotFound
}

// ListMembers returns all members of the given organisation.
func (s *OrgService) ListMembers(ctx context.Context, orgID string) ([]interfaces.OrgMember, error) {
	org, err := s.store.LoadOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return org.Members, nil
}

// GetAndCheckAdmin loads the organisation and verifies that userID is an admin member.
// Returns the organisation on success, or an error if the org doesn't exist or the
// user is not an admin.
func (s *OrgService) GetAndCheckAdmin(ctx context.Context, orgID, userID string) (*interfaces.Organisation, error) {
	org, err := s.store.LoadOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, m := range org.Members {
		if m.UserID == userID && m.Role == auth.RoleAdmin {
			return org, nil
		}
	}
	return nil, ErrNotOrgAdmin
}

// IsAdmin reports whether userID is an administrator of orgID.
func (s *OrgService) IsAdmin(ctx context.Context, orgID, userID string) bool {
	org, err := s.store.LoadOrg(ctx, orgID)
	if err != nil {
		return false
	}
	for _, m := range org.Members {
		if m.UserID == userID && m.Role == auth.RoleAdmin {
			return true
		}
	}
	return false
}

// Delete removes an organisation entirely.
func (s *OrgService) Delete(ctx context.Context, orgID string) error {
	return s.store.DeleteOrg(ctx, orgID)
}

func (s *OrgService) Update(ctx context.Context, orgID, name string) (*interfaces.Organisation, error) {
	if err := s.store.MutateOrg(ctx, orgID, func(org *interfaces.Organisation) error {
		org.Name = name
		org.UpdatedAt = time.Now().UTC()
		return nil
	}); err != nil {
		return nil, err
	}
	return s.store.LoadOrg(ctx, orgID)
}

func adminCount(org *interfaces.Organisation) int {
	n := 0
	for _, m := range org.Members {
		if m.Role == auth.RoleAdmin {
			n++
		}
	}
	return n
}

// CreateInvite creates a pending invite for email to join orgID with role,
// recorded as having been sent by invitedBy. It returns the stored invite
// record plus the raw invite token — the token is only ever available here;
// only its SHA-256 hash is persisted, so it must be delivered to the invitee
// out-of-band (e.g. embedded in an emailed accept link).
func (s *OrgService) CreateInvite(ctx context.Context, orgID, email string, role auth.Role, invitedBy string, ttl time.Duration) (*interfaces.OrgInvite, string, error) {
	if !role.IsValid() {
		return nil, "", fmt.Errorf("collaboration: invalid role %q", role)
	}
	if email == "" {
		return nil, "", errors.New("collaboration: email is required")
	}
	if ttl <= 0 {
		ttl = DefaultInviteTTL
	}

	token, tokenHash, err := generateInviteToken()
	if err != nil {
		return nil, "", err
	}

	now := time.Now().UTC()
	invite := &interfaces.OrgInvite{
		ID:        uuid.New().String(),
		OrgID:     orgID,
		Email:     email,
		Role:      role,
		InvitedBy: invitedBy,
		TokenHash: tokenHash,
		ExpiresAt: now.Add(ttl),
		CreatedAt: now,
	}

	if err := s.store.SaveOrgInvite(ctx, invite); err != nil {
		return nil, "", err
	}
	return invite, token, nil
}

// ListInvites returns all invites (pending and resolved) for an organisation.
func (s *OrgService) ListInvites(ctx context.Context, orgID string) ([]*interfaces.OrgInvite, error) {
	return s.store.ListOrgInvites(ctx, orgID)
}

// RevokeInvite deletes a pending invite, e.g. so it can no longer be accepted.
func (s *OrgService) RevokeInvite(ctx context.Context, orgID, inviteID string) error {
	if _, err := s.store.GetOrgInvite(ctx, orgID, inviteID); err != nil {
		if errors.Is(err, interfaces.ErrNotFound) {
			return ErrInviteNotFound
		}
		return err
	}
	if err := s.store.DeleteOrgInvite(ctx, orgID, inviteID); err != nil {
		if errors.Is(err, interfaces.ErrNotFound) {
			return ErrInviteNotFound
		}
		return err
	}
	return nil
}

// AcceptInvite validates the given raw invite token and, if valid and not
// expired or already used, adds userID as a member of the invite's
// organisation with the role specified in the invite. The caller's email
// must match the invite email to prevent token sharing attacks.
func (s *OrgService) AcceptInvite(ctx context.Context, token, userID, userEmail string) (*interfaces.Organisation, error) {
	if token == "" {
		return nil, ErrInviteNotFound
	}
	invite, err := s.store.GetOrgInviteByTokenHash(ctx, hashInviteToken(token))
	if err != nil {
		if errors.Is(err, interfaces.ErrNotFound) {
			return nil, ErrInviteNotFound
		}
		return nil, err
	}
	if invite.AcceptedAt != nil {
		return nil, ErrInviteAlreadyAccepted
	}
	if time.Now().UTC().After(invite.ExpiresAt) {
		return nil, ErrInviteExpired
	}
	// Require the caller to have a verified email claim and match the invite
	// email. The userEmail != "" guard used to silently SKIP the check when the
	// caller's JWT had no email — which meant anyone who obtained the invite
	// token (leaked via referrer, copy-paste, a mail-system backup) could accept
	// it and become a member with the invite's role (potentially admin). Reject
	// when the caller has no email rather than falling through.
	if userEmail == "" {
		return nil, ErrInviteNotFound
	}
	if invite.Email != userEmail {
		return nil, ErrInviteNotFound
	}

	if err := s.AddMember(ctx, invite.OrgID, userID, invite.Role); err != nil && !errors.Is(err, ErrAlreadyMember) {
		return nil, err
	}

	if err := s.store.MarkOrgInviteAccepted(ctx, invite.ID, time.Now().UTC()); err != nil {
		return nil, err
	}

	return s.store.LoadOrg(ctx, invite.OrgID)
}

// generateInviteToken returns a fresh random invite token and the hex-encoded
// SHA-256 hash that should be persisted in place of the token itself.
func generateInviteToken() (token, tokenHash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("collaboration: generate invite token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashInviteToken(token), nil
}

// hashInviteToken returns the hex-encoded SHA-256 hash of an invite token.
func hashInviteToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
