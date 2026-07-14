package testutil

import "sync"

// FakeSecretStore is an in-memory, per-(scope,provider) secret store for tests.
// It satisfies the service KeyStore / storage SecretStore method set
// (Save/Get/Has/Delete) structurally, so it can stand in for either.
type FakeSecretStore struct {
	mu   sync.Mutex
	keys map[string]string
}

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
	s.keys[s.key(scope, provider)] = key
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
	return nil
}
