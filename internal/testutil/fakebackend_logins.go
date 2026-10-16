package testutil

import (
	"context"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// RecordFailedLogin counts one wrong password under the lock, as the Postgres UPDATE does in one statement: an
// account locked at the time of the write is ErrAccountLocked and not counted.
func (m *FakeBackend) RecordFailedLogin(_ context.Context, userID string, threshold int, lockFor time.Duration) (int, *time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.Users[userID]
	if !ok {
		return 0, nil, interfaces.ErrNotFound
	}
	now := time.Now().UTC()
	if u.LockedUntil != nil && now.Before(*u.LockedUntil) {
		return 0, nil, interfaces.ErrAccountLocked
	}
	var attempts int
	var lockedUntil *time.Time
	m.updateUserLocked(userID, func(u *interfaces.User) {
		u.FailedLoginAttempts++
		if u.FailedLoginAttempts >= threshold {
			until := now.Add(lockFor)
			u.LockedUntil = &until
		}
		u.UpdatedAt = now
		attempts = u.FailedLoginAttempts
		if u.LockedUntil != nil {
			t := *u.LockedUntil
			lockedUntil = &t
		}
	})
	return attempts, lockedUntil, nil
}

// ResetFailedLogins clears the count and lock under the lock RecordFailedLogin takes: an account locked now is
// ErrAccountLocked and left as it is; a row already clear, or an unknown id, is left alone.
func (m *FakeBackend) ResetFailedLogins(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.Users[userID]
	if ok && u.LockedUntil != nil && time.Now().UTC().Before(*u.LockedUntil) {
		return interfaces.ErrAccountLocked
	}
	if !ok || (u.FailedLoginAttempts == 0 && u.LockedUntil == nil) {
		return nil
	}
	m.updateUserLocked(userID, func(u *interfaces.User) {
		u.FailedLoginAttempts, u.LockedUntil, u.UpdatedAt = 0, nil, time.Now().UTC()
	})
	return nil
}

// UpdateUserAvatarURL sets the avatar URL alone; an unknown id is ErrNotFound.
func (m *FakeBackend) UpdateUserAvatarURL(_ context.Context, id, avatarURL string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.updateUserLocked(id, func(u *interfaces.User) { u.AvatarURL, u.UpdatedAt = avatarURL, time.Now().UTC() }) {
		return interfaces.ErrNotFound
	}
	return nil
}
