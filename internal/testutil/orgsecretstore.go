package testutil

import (
	"fmt"
	"sort"
	"strings"
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

// savedBy reports whether userID lent the row's secret — secretSetBy,
// with updatedBy only for a row no save has stamped since v24 (the
// keystore's DeleteOrgIfSavedBy rule: an edit that carried the stored
// secret does not make its editor a lender).
func (r orgSecretRow) savedBy(userID string) bool {
	return userID != "" && (r.secretSetBy == userID || (r.secretSetBy == "" && r.updatedBy == userID))
}

// DeleteOrgIfSavedBy deletes the row only while userID is its lender
// (see savedBy).
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

// OrgsWithKeySavedBy lists the orgs whose provider row userID lent (see
// savedBy).
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

// ClearSecretSetBy empties a row's secret_set_by, standing in for a row
// no save has stamped since the v24 backfill (the disable path's
// updated_by fallback); a no-op on a missing row.
func (s *FakeOrgSecretStore) ClearSecretSetBy(orgID, provider string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[s.key(orgID, provider)]; ok {
		row.secretSetBy = ""
		s.rows[s.key(orgID, provider)] = row
	}
}

var _ storageif.OrgSecretFamilyStore = (*FakeOrgSecretStore)(nil)

// validOrgFamily mirrors the database keystore's family-name check.
func validOrgFamily(family string) error {
	if family == "" || strings.Contains(family, "/") {
		return fmt.Errorf("keystore: invalid key family %q", family)
	}
	return nil
}

// ListOrgKeyFamily returns the org's keys in family (the key itself or
// family/…), ordered by provider, with each row's metadata.
func (s *FakeOrgSecretStore) ListOrgKeyFamily(orgID, family string) ([]storageif.OrgKeyFamilyEntry, error) {
	if err := validOrgFamily(family); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []storageif.OrgKeyFamilyEntry{}
	for k, row := range s.rows {
		org, prov, ok := splitOrgKey(k)
		if !ok || org != orgID || !storageif.InSecretFamily(prov, family) {
			continue
		}
		out = append(out, storageif.OrgKeyFamilyEntry{Provider: prov, OrgKeyInfo: storageif.OrgKeyInfo{
			UpdatedBy: row.updatedBy, SecretSetBy: row.secretSetBy, UpdatedAt: row.updatedAt,
		}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out, nil
}

// OrgKeyFamilySavedBy lists every (org, key) in family that userID lent
// (see savedBy), ordered by org then provider.
func (s *FakeOrgSecretStore) OrgKeyFamilySavedBy(family, userID string) ([]storageif.OrgKeyRef, error) {
	if err := validOrgFamily(family); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storageif.OrgKeyRef
	for k, row := range s.rows {
		org, prov, ok := splitOrgKey(k)
		if ok && storageif.InSecretFamily(prov, family) && row.savedBy(userID) {
			out = append(out, storageif.OrgKeyRef{OrgID: org, Provider: prov})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OrgID != out[j].OrgID {
			return out[i].OrgID < out[j].OrgID
		}
		return out[i].Provider < out[j].Provider
	})
	return out, nil
}

// DeleteOrgKeyFamilyIfSavedBy deletes the org's keys in family that userID
// lent (see savedBy) and returns each with the document it held, ordered by
// provider — the database keystore's atomic DELETE … RETURNING.
func (s *FakeOrgSecretStore) DeleteOrgKeyFamilyIfSavedBy(orgID, family, userID string) ([]storageif.DeletedOrgKey, error) {
	if err := validOrgFamily(family); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storageif.DeletedOrgKey
	for k, row := range s.rows {
		org, prov, ok := splitOrgKey(k)
		if !ok || org != orgID || !storageif.InSecretFamily(prov, family) || !row.savedBy(userID) {
			continue
		}
		delete(s.rows, k)
		out = append(out, storageif.DeletedOrgKey{Provider: prov, Document: row.key})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out, nil
}

// DeleteOrgKeyIfUnchanged deletes the row only while its updatedAt is still
// updatedAt — the database keystore's conditional delete.
func (s *FakeOrgSecretStore) DeleteOrgKeyIfUnchanged(orgID, provider string, updatedAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(orgID, provider)]
	if !ok || !row.updatedAt.Equal(updatedAt) {
		return false, nil
	}
	delete(s.rows, s.key(orgID, provider))
	return true, nil
}

var _ storageif.OrgSecretRestorer = (*FakeOrgSecretStore)(nil)

// SnapshotOrg reads a row as stored (the fake seals nothing: Sealed is the
// document itself).
func (s *FakeOrgSecretStore) SnapshotOrg(orgID, provider string) (storageif.OrgKeySnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(orgID, provider)]
	if !ok {
		return storageif.OrgKeySnapshot{}, false, nil
	}
	return storageif.OrgKeySnapshot{
		OrgKeyInfo: storageif.OrgKeyInfo{UpdatedBy: row.updatedBy, SecretSetBy: row.secretSetBy, UpdatedAt: row.updatedAt},
		Sealed:     row.key,
	}, true, nil
}

// RestoreOrgIfUnchanged writes snap back (document, writer, lender, stamp)
// — or deletes the row for a nil snap — only while the row still carries
// current, like the database keystore.
func (s *FakeOrgSecretStore) RestoreOrgIfUnchanged(orgID, provider string, current time.Time, snap *storageif.OrgKeySnapshot) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(orgID, provider)]
	if !ok || !row.updatedAt.Equal(current) {
		return false, nil
	}
	if snap == nil {
		delete(s.rows, s.key(orgID, provider))
		return true, nil
	}
	s.rows[s.key(orgID, provider)] = orgSecretRow{key: snap.Sealed, updatedBy: snap.UpdatedBy, secretSetBy: snap.SecretSetBy, updatedAt: snap.UpdatedAt}
	return true, nil
}

// SetOrgKeyUpdatedAt back-dates (or post-dates) a row's last save, so a
// test can place it inside or outside the orphan sweep's grace period. It
// also moves the compare-and-swap token; a no-op on a missing row.
func (s *FakeOrgSecretStore) SetOrgKeyUpdatedAt(orgID, provider string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[s.key(orgID, provider)]; ok {
		row.updatedAt = at
		s.rows[s.key(orgID, provider)] = row
	}
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
