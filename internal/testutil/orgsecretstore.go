package testutil

import (
	"sort"
	"sync"
	"time"

	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// FakeOrgSecretStore is an in-memory, per-(org, provider) shared-credential
// store for tests. It satisfies the service OrgKeyStore / storage
// OrgSecretStore method set structurally, lender tracking included: a row
// records its last writer (updatedBy) and its secret's lender (secretSetBy).
type FakeOrgSecretStore struct {
	mu   sync.Mutex
	rows map[string]orgSecretRow
}

type orgSecretRow struct {
	key         string
	updatedBy   string
	secretSetBy string
	updatedAt   time.Time
}

// NewFakeOrgSecretStore returns an empty store ready for use.
func NewFakeOrgSecretStore() *FakeOrgSecretStore {
	return &FakeOrgSecretStore{rows: map[string]orgSecretRow{}}
}

func (s *FakeOrgSecretStore) key(orgID, provider string) string { return orgID + "\x00" + provider }

// SaveOrg upserts a row with a newly supplied secret: updatedBy is both the
// last writer and the lender.
func (s *FakeOrgSecretStore) SaveOrg(orgID, provider, key, updatedBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]orgSecretRow{}
	}
	s.rows[s.key(orgID, provider)] = orgSecretRow{key: key, updatedBy: updatedBy, secretSetBy: updatedBy, updatedAt: time.Now()}
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

func (s *FakeOrgSecretStore) OrgKeyMeta(orgID, provider string) (storageif.OrgKeyInfo, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(orgID, provider)]
	if !ok {
		return storageif.OrgKeyInfo{}, false, nil
	}
	return storageif.OrgKeyInfo{UpdatedBy: row.updatedBy, SecretSetBy: row.secretSetBy, UpdatedAt: row.updatedAt}, true, nil
}

// SaveOrgIfUnchanged is SaveOrg with compare-and-swap on updatedAt, like
// the database keystore: a zero expected writes only when there is no row.
// secretCarried keeps the row's lender; otherwise updatedBy becomes it.
func (s *FakeOrgSecretStore) SaveOrgIfUnchanged(orgID, provider, key, updatedBy string, secretCarried bool, expected time.Time) (time.Time, bool, error) {
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
	lender := updatedBy
	if secretCarried && exists {
		lender = row.secretSetBy
	}
	s.rows[s.key(orgID, provider)] = orgSecretRow{key: key, updatedBy: updatedBy, secretSetBy: lender, updatedAt: now}
	return now, true, nil
}

// savedBy reports whether userID lent the row's secret or last wrote it.
func (r orgSecretRow) savedBy(userID string) bool {
	return userID != "" && (r.updatedBy == userID || r.secretSetBy == userID)
}

// DeleteOrgIfSavedBy deletes the row only while userID is its lender or its
// last writer.
func (s *FakeOrgSecretStore) DeleteOrgIfSavedBy(orgID, provider, userID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(orgID, provider)]
	if !ok || !row.savedBy(userID) {
		return false, nil
	}
	delete(s.rows, s.key(orgID, provider))
	return true, nil
}

// OrgsWithKeySavedBy lists the orgs whose provider row userID lent or last
// wrote.
func (s *FakeOrgSecretStore) OrgsWithKeySavedBy(provider, userID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, row := range s.rows {
		orgID, prov, ok := splitOrgKey(k)
		if ok && prov == provider && row.savedBy(userID) {
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
