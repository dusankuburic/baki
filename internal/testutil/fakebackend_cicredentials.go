package testutil

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// The organisations' CI credentials (org_ci_credentials, migration 0038), as the Postgres store keeps them: the
// organisation must exist (the foreign key), goes with its credentials (ON DELETE CASCADE), and a backend without
// field encryption (FieldEncryptionOff) refuses to store a secret at all.

// ciTouchThrottle is how long a recorded use keeps the next one from rewriting last_used_at.
const ciTouchThrottle = 5 * time.Minute

func cloneCICredential(c *interfaces.CICredential, withSecret bool) *interfaces.CICredential {
	cp := *c
	if c.ExpiresAt != nil {
		t := *c.ExpiresAt
		cp.ExpiresAt = &t
	}
	if c.LastUsedAt != nil {
		t := *c.LastUsedAt
		cp.LastUsedAt = &t
	}
	if !withSecret {
		cp.Secret = ""
	}
	return &cp
}

func (m *FakeBackend) CreateCICredential(_ context.Context, c *interfaces.CICredential, maxActive int) error {
	if c == nil || c.ID == "" || c.OrgID == "" || c.Secret == "" {
		return errors.New("ci credential requires id, orgId and secret")
	}
	if m.FieldEncryptionOff {
		return interfaces.ErrSecretSealingUnavailable
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Orgs[c.OrgID] == nil {
		return errForeignKey("org_ci_credentials", "organisation", c.OrgID)
	}
	if _, dup := m.ciCredentials[c.ID]; dup {
		return errors.New("fake: duplicate key value violates unique constraint \"org_ci_credentials_pkey\"")
	}
	active := 0
	for _, cur := range m.ciCredentials {
		if cur.OrgID == c.OrgID && cur.Active(now) {
			active++
		}
	}
	if active >= maxActive {
		return interfaces.ErrCICredentialLimit
	}
	if m.ciCredentials == nil {
		m.ciCredentials = map[string]*interfaces.CICredential{}
	}
	row := cloneCICredential(c, true)
	row.LastUsedAt = nil
	m.ciCredentials[c.ID] = row
	return nil
}

func (m *FakeBackend) GetCICredential(_ context.Context, id string) (*interfaces.CICredential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c, ok := m.ciCredentials[id]; ok {
		return cloneCICredential(c, true), nil
	}
	return nil, interfaces.ErrNotFound
}

// ListCICredentials is newest first, then by id, without secrets.
func (m *FakeBackend) ListCICredentials(_ context.Context, orgID string) ([]*interfaces.CICredential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*interfaces.CICredential{}
	for _, c := range m.ciCredentials {
		if c.OrgID == orgID {
			out = append(out, cloneCICredential(c, false))
		}
	}
	slices.SortFunc(out, func(a, b *interfaces.CICredential) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	return out, nil
}

// ActiveCICredentials is the unexpired credentials, the most recently used first (never used last), then newest.
func (m *FakeBackend) ActiveCICredentials(_ context.Context, orgID string, limit int) ([]*interfaces.CICredential, error) {
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*interfaces.CICredential{}
	for _, c := range m.ciCredentials {
		if c.OrgID == orgID && c.Active(now) {
			out = append(out, cloneCICredential(c, true))
		}
	}
	lastUsed := func(c *interfaces.CICredential) time.Time {
		if c.LastUsedAt == nil {
			return time.Time{}
		}
		return *c.LastUsedAt
	}
	slices.SortFunc(out, func(a, b *interfaces.CICredential) int {
		return cmp.Or(lastUsed(b).Compare(lastUsed(a)), b.CreatedAt.Compare(a.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *FakeBackend) DeleteCICredential(_ context.Context, orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.ciCredentials[id]; !ok || c.OrgID != orgID {
		return interfaces.ErrNotFound
	}
	delete(m.ciCredentials, id)
	return nil
}

func (m *FakeBackend) TouchCICredential(_ context.Context, id string) error {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.ciCredentials[id]; ok && (c.LastUsedAt == nil || c.LastUsedAt.Before(now.Add(-ciTouchThrottle))) {
		c.LastUsedAt = &now
	}
	return nil
}

func (m *FakeBackend) LoadFlowHeaderInOrg(ctx context.Context, id, orgID string) (*interfaces.FlowDocument, error) {
	h, err := m.LoadFlowHeader(ctx, id)
	if err != nil {
		return nil, err
	}
	if orgID == "" || h.OrganizationID != orgID {
		return nil, interfaces.ErrNotFound
	}
	return h, nil
}

// SetCICredentialLastUsed sets a credential's last use (nil: never), for tests of the throttle and the ordering.
func (m *FakeBackend) SetCICredentialLastUsed(id string, at *time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.ciCredentials[id]; ok {
		c.LastUsedAt = at
	}
}

// deleteOrgCICredentialsLocked is the foreign key's cascade when the organisation goes. The caller holds m.mu.
func (m *FakeBackend) deleteOrgCICredentialsLocked(orgID string) {
	deleteWhere(m.ciCredentials, func(c *interfaces.CICredential) bool { return c.OrgID == orgID })
}

// eraseCICredentialCreatorLocked is the erase step: the user's credentials stay their organisation's, without the
// minter. The caller holds m.mu.
func (m *FakeBackend) eraseCICredentialCreatorLocked(userID string) {
	for _, c := range m.ciCredentials {
		if c.CreatedBy == userID {
			c.CreatedBy = ""
		}
	}
}

// purgeCICredentialsLocked is the retention step: credentials expired more than 30 days before now. The caller holds
// m.mu.
func (m *FakeBackend) purgeCICredentialsLocked(now time.Time) int {
	cutoff := now.AddDate(0, 0, -30)
	return deleteWhere(m.ciCredentials, func(c *interfaces.CICredential) bool {
		return c.ExpiresAt != nil && c.ExpiresAt.Before(cutoff)
	})
}

// ciDelivery is one row of the CI webhook's replay record (ci_webhook_deliveries, migration 0039).
type ciDelivery struct{ caller, id string }

// ciDeliveryRetention is the retention sweep's horizon for the replay record.
const ciDeliveryRetention = time.Hour

// RecordCIDelivery is the primary key's ON CONFLICT DO NOTHING: under m.mu, of two records of one pair the second
// finds the first. Like the Postgres statement it first removes the caller's records older than the retention.
func (m *FakeBackend) RecordCIDelivery(_ context.Context, caller, deliveryID string) (bool, error) {
	if n := len(deliveryID); n < 16 || n > 128 {
		return false, errors.New("fake: new row violates check constraint \"ci_webhook_deliveries_id_check\"")
	}
	key := ciDelivery{caller, deliveryID}
	cutoff := time.Now().Add(-ciDeliveryRetention)
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.ciDeliveries, func(k ciDelivery, at time.Time) bool { return k.caller == caller && at.Before(cutoff) })
	if _, dup := m.ciDeliveries[key]; dup {
		return false, nil
	}
	if m.ciDeliveries == nil {
		m.ciDeliveries = map[ciDelivery]time.Time{}
	}
	m.ciDeliveries[key] = time.Now()
	return true, nil
}

// SetCIDeliveryReceived backdates a recorded delivery, for tests of the retention sweep.
func (m *FakeBackend) SetCIDeliveryReceived(caller, deliveryID string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ciDeliveries[ciDelivery{caller, deliveryID}]; ok {
		m.ciDeliveries[ciDelivery{caller, deliveryID}] = at
	}
}

// purgeCIDeliveriesLocked is the retention step: records older than an hour. The caller holds m.mu.
func (m *FakeBackend) purgeCIDeliveriesLocked(now time.Time) int {
	n := len(m.ciDeliveries)
	maps.DeleteFunc(m.ciDeliveries, func(_ ciDelivery, at time.Time) bool { return at.Before(now.Add(-ciDeliveryRetention)) })
	return n - len(m.ciDeliveries)
}
