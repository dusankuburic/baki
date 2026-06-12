package auth

import (
	"sync"
	"time"
)

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
