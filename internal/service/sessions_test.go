package service

import (
	"context"
	"errors"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// memRefreshStore is an in-memory RefreshTokenStore.
type memRefreshStore struct {
	tokens     map[string]storageif.RefreshTokenInfo
	revoked    map[string]bool
	revokedAll []string
	verifyErr  error
}

func newMemRefreshStore() *memRefreshStore {
	return &memRefreshStore{tokens: map[string]storageif.RefreshTokenInfo{}, revoked: map[string]bool{}}
}

func (m *memRefreshStore) StoreRefreshToken(_ context.Context, jti, userID string, expiresAt time.Time, _, _ string) error {
	m.tokens[jti] = storageif.RefreshTokenInfo{ID: jti, UserID: userID, ExpiresAt: expiresAt}
	return nil
}

func (m *memRefreshStore) IsRefreshTokenValid(_ context.Context, jti string) (bool, error) {
	_, ok := m.tokens[jti]
	return ok && !m.revoked[jti], nil
}

func (m *memRefreshStore) RevokeRefreshToken(_ context.Context, jti string) error {
	m.revoked[jti] = true
	return nil
}

func (m *memRefreshStore) VerifyAndRevokeRefreshToken(_ context.Context, jti string) (*storageif.RefreshTokenInfo, error) {
	if m.verifyErr != nil {
		return nil, m.verifyErr
	}
	info, ok := m.tokens[jti]
	if !ok || m.revoked[jti] {
		return nil, storageif.ErrTokenAlreadyRevoked
	}
	m.revoked[jti] = true
	return &info, nil
}

func (m *memRefreshStore) RevokeUserRefreshTokens(_ context.Context, userID string) error {
	m.revokedAll = append(m.revokedAll, userID)
	return nil
}

func (m *memRefreshStore) ListUserRefreshTokens(context.Context, string) ([]storageif.RefreshTokenInfo, error) {
	return nil, nil
}

func (m *memRefreshStore) RevokeRefreshTokenForUser(_ context.Context, jti, userID string) error {
	if info, ok := m.tokens[jti]; !ok || info.UserID != userID {
		return storageif.ErrNotFound
	}
	m.revoked[jti] = true
	return nil
}

func TestSessionService_Enabled(t *testing.T) {
	var nilSvc *SessionService
	if nilSvc.Enabled() || NewSessionService(nil).Enabled() {
		t.Error("no store must not be enabled")
	}
	if !NewSessionService(newMemRefreshStore()).Enabled() {
		t.Error("a store must be enabled")
	}
}

// Rotation consumes the token once; a replay revokes every session of the user, a token stored for another user
// is refused, and a store failure is passed through.
func TestSessionService_Rotate(t *testing.T) {
	ctx := context.Background()
	store := newMemRefreshStore()
	svc := NewSessionService(store)
	_ = svc.Record(ctx, "j1", "u1", time.Now().Add(time.Hour), "ua", "ip")
	_ = svc.Record(ctx, "j2", "u2", time.Now().Add(time.Hour), "ua", "ip")

	if err := svc.Rotate(ctx, "u1", "j1"); err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	if err := svc.Rotate(ctx, "u1", "j1"); !errors.Is(err, ErrSessionReplayed) {
		t.Errorf("replay: %v, want ErrSessionReplayed", err)
	}
	if len(store.revokedAll) != 1 || store.revokedAll[0] != "u1" {
		t.Errorf("replay revoked %v, want every session of u1", store.revokedAll)
	}
	if err := svc.Rotate(ctx, "u1", "j2"); !errors.Is(err, ErrSessionUserMismatch) {
		t.Errorf("foreign token: %v, want ErrSessionUserMismatch", err)
	}
	store.verifyErr = errors.New("db down")
	if err := svc.Rotate(ctx, "u1", "j3"); err == nil || errors.Is(err, ErrSessionReplayed) || errors.Is(err, ErrSessionUserMismatch) {
		t.Errorf("store failure: %v, want the store's error", err)
	}
}

func TestSessionService_ListNeverNil(t *testing.T) {
	sessions, err := NewSessionService(newMemRefreshStore()).List(context.Background(), "u1")
	if err != nil || sessions == nil {
		t.Errorf("List = %v, %v; want an empty, non-nil list", sessions, err)
	}
}
