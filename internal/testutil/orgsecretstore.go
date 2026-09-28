package testutil

import (
	"sort"
	"sync"
	"time"

	"pad-analyzer/internal/storage"
)

// FakeOrgSecretStore is an in-memory, per-(org, provider) shared-credential
// store for tests. It satisfies the service OrgKeyStore / storage
// OrgSecretStore method set structurally.
type FakeOrgSecretStore struct {
	mu   sync.Mutex
	rows map[string]orgSecretRow
}

type orgSecretRow struct {
	key       string
	updatedBy string
	updatedAt time.Time
}

// NewFakeOrgSecretStore returns an empty store ready for use.
func NewFakeOrgSecretStore() *FakeOrgSecretStore {
	return &FakeOrgSecretStore{rows: map[string]orgSecretRow{}}
}

func (s *FakeOrgSecretStore) key(orgID, provider string) string { return orgID + "\x00" + provider }

func (s *FakeOrgSecretStore) SaveOrg(orgID, provider, key, updatedBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]orgSecretRow{}
	}
	s.rows[s.key(orgID, provider)] = orgSecretRow{key: key, updatedBy: updatedBy, updatedAt: time.Now()}
	return nil
}

// GetOrg returns storage.ErrSecretNotFound for a missing row, like the
// database keystore.
func (s *FakeOrgSecretStore) GetOrg(orgID, provider string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(orgID, provider)]
	if !ok {
		return "", storage.ErrSecretNotFound
	}
	return row.key, nil
}

func (s *FakeOrgSecretStore) HasOrg(orgID, provider string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rows[s.key(orgID, provider)]
	return ok, nil
}

func (s *FakeOrgSecretStore) DeleteOrg(orgID, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, s.key(orgID, provider))
	return nil
}

func (s *FakeOrgSecretStore) OrgKeyMeta(orgID, provider string) (string, time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(orgID, provider)]
	if !ok {
		return "", time.Time{}, false, nil
	}
	return row.updatedBy, row.updatedAt, true, nil
}

// SaveOrgIfUnchanged is SaveOrg with compare-and-swap on updatedAt, like
// the database keystore: a zero expected writes only when there is no row.
func (s *FakeOrgSecretStore) SaveOrgIfUnchanged(orgID, provider, key, updatedBy string, expected time.Time) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]orgSecretRow{}
	}
	row, exists := s.rows[s.key(orgID, provider)]
	if expected.IsZero() && exists || !expected.IsZero() && (!exists || !row.updatedAt.Equal(expected)) {
		return time.Time{}, false, nil
	}
	now := time.Now()
	if exists && !now.After(row.updatedAt) {
		now = row.updatedAt.Add(time.Microsecond) // always a new stamp
	}
	s.rows[s.key(orgID, provider)] = orgSecretRow{key: key, updatedBy: updatedBy, updatedAt: now}
	return now, true, nil
}

// DeleteOrgIfUpdatedBy deletes the row only while updatedBy last wrote it.
func (s *FakeOrgSecretStore) DeleteOrgIfUpdatedBy(orgID, provider, updatedBy string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(orgID, provider)]
	if !ok || row.updatedBy != updatedBy {
		return false, nil
	}
	delete(s.rows, s.key(orgID, provider))
	return true, nil
}

// OrgsWithKeyUpdatedBy lists the orgs whose provider row updatedBy wrote.
func (s *FakeOrgSecretStore) OrgsWithKeyUpdatedBy(provider, updatedBy string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, row := range s.rows {
		orgID, prov, ok := splitOrgKey(k)
		if ok && prov == provider && row.updatedBy == updatedBy {
			out = append(out, orgID)
		}
	}
	sort.Strings(out)
	return out, nil
}

func splitOrgKey(k string) (orgID, provider string, ok bool) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return k[:i], k[i+1:], true
		}
	}
	return "", "", false
}

// OrgKeyLookup adapts the store to the ProviderFactory's org lookup
// contract (missing = "", nil).
func (s *FakeOrgSecretStore) OrgKeyLookup(orgID, provider string) (string, error) {
	v, err := s.GetOrg(orgID, provider)
	if err != nil {
		return "", nil
	}
	return v, nil
}
