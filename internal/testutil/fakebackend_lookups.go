package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// The point lookups, the batch audit write, the streamed export and identity links, which were optional
// capabilities of the Postgres backend before they joined the role interfaces (refresh tokens are in
// fakebackend.go). Each follows the Postgres method it mirrors (internal/storage/contract holds both
// to it), built on the fake's own methods where one serves.

func (m *FakeBackend) GetFindingStatus(ctx context.Context, flowID, findingKey string) (*interfaces.FindingStatus, error) {
	statuses, err := m.ListFindingStatuses(ctx, flowID)
	if err != nil {
		return nil, err
	}
	for _, st := range statuses {
		if st.FindingKey == findingKey {
			cp := *st
			return &cp, nil
		}
	}
	return nil, nil
}

// GetAPITokenWithOwnerByHash is the token and its owner's user row, as Postgres's join projects it (no password
// hash); a token whose owner is gone is not found.
func (m *FakeBackend) GetAPITokenWithOwnerByHash(ctx context.Context, tokenHash string) (*interfaces.APIToken, *interfaces.User, error) {
	tok, err := m.GetAPITokenByHash(ctx, tokenHash)
	if err != nil {
		return nil, nil, err
	}
	user, err := m.LoadUserByID(ctx, tok.UserID)
	if err != nil {
		return nil, nil, err
	}
	owner := *user
	owner.Password = ""
	return tok, &owner, nil
}

func (m *FakeBackend) OrgMemberRole(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	org, err := m.LoadOrg(ctx, orgID)
	if err != nil {
		return "", false, err
	}
	for _, mem := range org.Members {
		if mem.UserID == userID {
			return mem.Role, true, nil
		}
	}
	return "", false, nil
}

func (m *FakeBackend) ListOrgMembershipsForUser(ctx context.Context, userID string) ([]interfaces.OrgMembership, error) {
	orgs, err := m.ListOrgsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	type row struct {
		m       interfaces.OrgMembership
		updated time.Time
	}
	var rows []row
	for _, org := range orgs {
		for _, mem := range org.Members {
			if mem.UserID == userID {
				rows = append(rows, row{interfaces.OrgMembership{OrgID: org.ID, OrgName: org.Name, Role: mem.Role}, org.UpdatedAt})
				break
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].updated.Equal(rows[j].updated) {
			return rows[i].updated.After(rows[j].updated)
		}
		return rows[i].m.OrgID < rows[j].m.OrgID
	})
	out := make([]interfaces.OrgMembership, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.m)
	}
	return out, nil
}

// ListFlowsWithTotal is ListFlows and CountFlows of the same filter. Like Postgres, which counts on the page's rows,
// it counts again only for an empty page past the first.
func (m *FakeBackend) ListFlowsWithTotal(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, int, error) {
	docs, err := m.ListFlows(ctx, filter)
	if err != nil || (len(docs) == 0 && filter.Offset <= 0) {
		return docs, 0, err
	}
	total, err := m.CountFlows(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	return docs, total, nil
}

// SaveAuditEvents keeps every row through SaveAuditEvent, all or none: as the primary key does on Postgres, an id
// already saved, or twice in the batch, fails the whole batch.
func (m *FakeBackend) SaveAuditEvents(ctx context.Context, events []*interfaces.AuditEvent) error {
	ids := map[string]bool{}
	for _, e := range m.SavedAuditEvents() {
		ids[e.ID] = true
	}
	for i, e := range events {
		if e == nil {
			return fmt.Errorf("save audit events: event %d is nil", i)
		}
		if ids[e.ID] {
			return fmt.Errorf("save audit events: duplicate id %q", e.ID)
		}
		ids[e.ID] = true
	}
	for _, e := range events {
		if err := m.SaveAuditEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// ExportUserDataTo writes ExportUserData's bundle as one JSON object, as the Postgres stream does.
func (m *FakeBackend) ExportUserDataTo(ctx context.Context, userID string, w io.Writer) error {
	export, err := m.ExportUserData(ctx, userID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(export)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// fakeIdentityLinks holds the fake's identity links by provider and subject; the zero value is ready.
type fakeIdentityLinks struct {
	mu    sync.Mutex
	links map[string]*interfaces.IdentityLink
}

func identityKey(provider, subject string) string { return provider + "\x00" + subject }

// deleteUser is the erasure's cascade (identity_links.user_id ON DELETE CASCADE).
func (l *fakeIdentityLinks) deleteUser(userID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, link := range l.links {
		if link.UserID == userID {
			delete(l.links, k)
		}
	}
}

func (m *FakeBackend) SaveIdentityLink(_ context.Context, link *interfaces.IdentityLink) error {
	l := &m.identityLinks
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.links == nil {
		l.links = make(map[string]*interfaces.IdentityLink)
	}
	k := identityKey(link.Provider, link.Subject)
	if _, ok := l.links[k]; ok {
		return fmt.Errorf("identity link already exists for %s subject", link.Provider)
	}
	cp := *link
	cp.CreatedAt = time.Now().UTC()
	l.links[k] = &cp
	return nil
}

func (m *FakeBackend) LoadIdentityLink(_ context.Context, provider, subject string) (*interfaces.IdentityLink, error) {
	l := &m.identityLinks
	l.mu.Lock()
	defer l.mu.Unlock()
	link, ok := l.links[identityKey(provider, subject)]
	if !ok {
		return nil, interfaces.ErrNotFound
	}
	cp := *link
	return &cp, nil
}

func (m *FakeBackend) ListIdentityLinks(_ context.Context, userID string) ([]*interfaces.IdentityLink, error) {
	l := &m.identityLinks
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []*interfaces.IdentityLink{}
	for _, link := range l.links {
		if link.UserID == userID {
			cp := *link
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return identityKey(out[i].Provider, out[i].Subject) < identityKey(out[j].Provider, out[j].Subject)
	})
	return out, nil
}

func (m *FakeBackend) DeleteIdentityLink(_ context.Context, userID, provider string) error {
	l := &m.identityLinks
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, link := range l.links {
		if link.UserID == userID && link.Provider == provider {
			delete(l.links, k)
		}
	}
	return nil
}
