package auth

import (
	"sync"
	"time"
)

// BlacklistStore is the storage abstraction for access-token revocation.
// The in-memory implementation (TokenBlacklist) is used in local mode and
// tests; a Postgres-backed implementation is used in cloud mode so that a
// logout on one replica is immediately visible to all others.
type BlacklistStore interface {
	Add(jti string, ttl time.Duration)
	IsRevoked(jti string) bool
	AddIfAbsent(jti string, ttl time.Duration) bool
	Stop()
}

type TokenBlacklist struct {
	mu      sync.RWMutex
	entries map[string]time.Time
	stopCh  chan struct{}
}

func NewTokenBlacklist() *TokenBlacklist {
	bl := &TokenBlacklist{
		entries: make(map[string]time.Time),
		stopCh:  make(chan struct{}),
	}
	go bl.cleanup()
	return bl
}

func (bl *TokenBlacklist) Add(jti string, ttl time.Duration) {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	bl.entries[jti] = time.Now().Add(ttl)
}

func (bl *TokenBlacklist) IsRevoked(jti string) bool {
	bl.mu.RLock()
	defer bl.mu.RUnlock()
	expiry, ok := bl.entries[jti]
	if !ok {
		return false
	}
	return time.Now().Before(expiry)
}

// AddIfAbsent atomically checks whether jti is already blacklisted and, if
// not, inserts it under a single write lock. Returns true if the entry was
// freshly added, false if it was already present. This eliminates the TOCTOU
// race between a separate IsRevoked + Add sequence.
func (bl *TokenBlacklist) AddIfAbsent(jti string, ttl time.Duration) bool {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	if exp, ok := bl.entries[jti]; ok && time.Now().Before(exp) {
		return false
	}
	bl.entries[jti] = time.Now().Add(ttl)
	return true
}

func (bl *TokenBlacklist) Stop() {
	close(bl.stopCh)
}

func (bl *TokenBlacklist) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-bl.stopCh:
			return
		case <-ticker.C:
			bl.mu.Lock()
			now := time.Now()
			for jti, expiry := range bl.entries {
				if now.After(expiry) {
					delete(bl.entries, jti)
				}
			}
			bl.mu.Unlock()
		}
	}
}
