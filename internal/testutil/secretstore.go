package testutil

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// FakeSecretStore is an in-memory, per-(scope,provider) secret store for tests.
// It satisfies the service KeyStore / storage SecretStore method set
// (Save/Get/Has/Delete) structurally, so it can stand in for either, and
// lists key families like the database keystore (storageif.SecretFamilyLister).
type FakeSecretStore struct {
	mu      sync.Mutex
	keys    map[string]string
	updated map[string]time.Time // each key's last save, for ListFamily
}

var _ storageif.SecretFamilyLister = (*FakeSecretStore)(nil)

// NewFakeSecretStore returns an empty store ready for use.
func NewFakeSecretStore() *FakeSecretStore {
	return &FakeSecretStore{keys: map[string]string{}}
}

func (s *FakeSecretStore) key(scope, provider string) string { return scope + "\x00" + provider }

func (s *FakeSecretStore) Save(scope, provider, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = map[string]string{}
	}
	if s.updated == nil {
		s.updated = map[string]time.Time{}
	}
	s.keys[s.key(scope, provider)] = key
	s.updated[s.key(scope, provider)] = time.Now()
	return nil
}

func (s *FakeSecretStore) Get(scope, provider string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[s.key(scope, provider)], nil
}

func (s *FakeSecretStore) Has(scope, provider string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.keys[s.key(scope, provider)]
	return ok, nil
}

func (s *FakeSecretStore) Delete(scope, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, s.key(scope, provider))
	delete(s.updated, s.key(scope, provider))
	return nil
}

// ListFamily returns scope's keys in family (the key itself or family/…),
// ordered by provider, with each key's last save time — the database
// keystore's contract, invalid family names included.
func (s *FakeSecretStore) ListFamily(scope, family string) ([]storageif.SecretFamilyEntry, error) {
	if family == "" || strings.Contains(family, "/") {
		return nil, fmt.Errorf("keystore: invalid key family %q", family)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []storageif.SecretFamilyEntry{}
	prefix := scope + "\x00"
	for k := range s.keys {
		provider, ok := strings.CutPrefix(k, prefix)
		if !ok || !storageif.InSecretFamily(provider, family) {
			continue
		}
		out = append(out, storageif.SecretFamilyEntry{Provider: provider, UpdatedAt: s.updated[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out, nil
}

// DeleteIfUnchanged deletes scope's key only while its last save is still
// updatedAt (the time ListFamily reported) — the database keystore's
// conditional delete: a key saved again since is kept.
func (s *FakeSecretStore) DeleteIfUnchanged(scope, provider string, updatedAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.key(scope, provider)
	if _, ok := s.keys[k]; !ok || !s.updated[k].Equal(updatedAt) {
		return false, nil
	}
	delete(s.keys, k)
	delete(s.updated, k)
	return true, nil
}

// SetUpdatedAt back-dates (or post-dates) a stored key's last save, so a
// test can place it inside or outside the orphan sweep's grace period; a
// no-op for a missing key.
func (s *FakeSecretStore) SetUpdatedAt(scope, provider string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[s.key(scope, provider)]; !ok {
		return
	}
	if s.updated == nil {
		s.updated = map[string]time.Time{}
	}
	s.updated[s.key(scope, provider)] = at
}
