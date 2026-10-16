package testutil

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// The organisation service's store beyond SaveOrg and the reads (collaboration.OrgStore): the locked update and
// the invitations, as the Postgres backend keeps them (postgres_orgs.go, postgres_invites.go).

// reconcileRoster is the roster a save stores: members, in their order, each keeping the JoinedAt it joined with
// (from existing), a new member's zero JoinedAt being now. A user listed twice fails, as the multi-row upsert does.
func reconcileRoster(existing, members []interfaces.OrgMember, now time.Time) ([]interfaces.OrgMember, error) {
	joined := make(map[string]time.Time, len(existing))
	for _, mem := range existing {
		joined[mem.UserID] = mem.JoinedAt
	}
	seen := make(map[string]bool, len(members))
	var out []interfaces.OrgMember
	for _, mem := range members {
		if seen[mem.UserID] {
			return nil, fmt.Errorf("upsert members: user %q listed twice", mem.UserID)
		}
		seen[mem.UserID] = true
		if j, ok := joined[mem.UserID]; ok {
			mem.JoinedAt = j
		} else if mem.JoinedAt.IsZero() {
			mem.JoinedAt = now
		}
		out = append(out, mem)
	}
	return out, nil
}

// MutateOrg applies fn to a copy of the stored organisation and stores its name, owner, UpdatedAt (as fn leaves it)
// and roster (reconcileRoster); the id and CreatedAt stay. An error from fn writes nothing; a missing organisation is
// ErrNotFound. fn runs under the fake's lock, as Postgres runs it holding the row: it must not call the fake.
func (m *FakeBackend) MutateOrg(_ context.Context, id string, fn func(*interfaces.Organisation) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	org, ok := m.Orgs[id]
	if !ok {
		return interfaces.ErrNotFound
	}
	cp := cloneOrg(org)
	if err := fn(cp); err != nil {
		return err
	}
	members, err := reconcileRoster(org.Members, cp.Members, time.Now().UTC())
	if err != nil {
		return err
	}
	m.Orgs[id] = &interfaces.Organisation{
		ID: org.ID, Name: cp.Name, OwnerID: cp.OwnerID, Members: members, CreatedAt: org.CreatedAt, UpdatedAt: cp.UpdatedAt,
	}
	return nil
}

func cloneInvite(inv *interfaces.OrgInvite) *interfaces.OrgInvite {
	cp := *inv
	if inv.AcceptedAt != nil {
		at := *inv.AcceptedAt
		cp.AcceptedAt = &at
	}
	return &cp
}

// SeedOrgs stores an organisation without members for each id the fake does not hold yet, and leaves the others as
// they are: a test's policies, channels, custom rules and knowledge base need their organisation, as the foreign keys
// of migration 0048 make Postgres ask.
func (m *FakeBackend) SeedOrgs(ids ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Orgs == nil {
		m.Orgs = make(map[string]*interfaces.Organisation)
	}
	now := time.Now().UTC()
	for _, id := range ids {
		if _, ok := m.Orgs[id]; !ok {
			m.Orgs[id] = &interfaces.Organisation{ID: id, Name: id, CreatedAt: now, UpdatedAt: now}
		}
	}
}

// SaveOrgInvite inserts the invite, stamping a zero CreatedAt (on invite): a taken id or token hash, or a second
// pending invite for the organisation and email, is ErrOrgInviteExists (the primary key and the unique indexes), and
// the organisation must exist (the foreign key). A pending invite for the organisation and email that has expired
// gives way: it is deleted, but only when the new one is stored, as the Postgres transaction rolls the delete back.
func (m *FakeBackend) SaveOrgInvite(_ context.Context, invite *interfaces.OrgInvite) error {
	if invite.CreatedAt.IsZero() {
		invite.CreatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var stale []string
	if invite.AcceptedAt == nil {
		for id, inv := range m.OrgInvites {
			if inv.OrgID == invite.OrgID && inv.Email == invite.Email && inv.AcceptedAt == nil && inv.ExpiresAt.Before(now) {
				stale = append(stale, id)
			}
		}
	}
	for id, inv := range m.OrgInvites {
		if slices.Contains(stale, id) {
			continue
		}
		if id == invite.ID || inv.TokenHash == invite.TokenHash ||
			(invite.AcceptedAt == nil && inv.OrgID == invite.OrgID && inv.Email == invite.Email && inv.AcceptedAt == nil) {
			return interfaces.ErrOrgInviteExists
		}
	}
	if _, ok := m.Orgs[invite.OrgID]; !ok {
		return errForeignKey("org_invites", "organisation", invite.OrgID)
	}
	if m.OrgInvites == nil {
		m.OrgInvites = make(map[string]*interfaces.OrgInvite)
	}
	for _, id := range stale {
		delete(m.OrgInvites, id)
	}
	m.OrgInvites[invite.ID] = cloneInvite(invite)
	return nil
}

// ListOrgInvites returns the organisation's invites newest first, id breaking ties (nil when it has none).
func (m *FakeBackend) ListOrgInvites(_ context.Context, orgID string) ([]*interfaces.OrgInvite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*interfaces.OrgInvite
	for _, inv := range m.OrgInvites {
		if inv.OrgID == orgID {
			out = append(out, cloneInvite(inv))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *FakeBackend) GetOrgInvite(_ context.Context, orgID, inviteID string) (*interfaces.OrgInvite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if inv, ok := m.OrgInvites[inviteID]; ok && inv.OrgID == orgID {
		return cloneInvite(inv), nil
	}
	return nil, interfaces.ErrNotFound
}

func (m *FakeBackend) GetOrgInviteByTokenHash(_ context.Context, tokenHash string) (*interfaces.OrgInvite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, inv := range m.OrgInvites {
		if inv.TokenHash == tokenHash {
			return cloneInvite(inv), nil
		}
	}
	return nil, interfaces.ErrNotFound
}

func (m *FakeBackend) DeleteOrgInvite(_ context.Context, orgID, inviteID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inv, ok := m.OrgInvites[inviteID]; ok && inv.OrgID == orgID {
		delete(m.OrgInvites, inviteID)
		return nil
	}
	return interfaces.ErrNotFound
}

// MarkOrgInviteAccepted stamps acceptedAt on a pending invite; an accepted or missing one is
// ErrOrgInviteAlreadyAccepted (Postgres's guarded UPDATE matches neither).
func (m *FakeBackend) MarkOrgInviteAccepted(_ context.Context, inviteID string, acceptedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.OrgInvites[inviteID]
	if !ok || inv.AcceptedAt != nil {
		return interfaces.ErrOrgInviteAlreadyAccepted
	}
	at := acceptedAt
	inv.AcceptedAt = &at
	return nil
}
