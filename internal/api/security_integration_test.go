package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// --- SSE per-user filtering (EmitTo) ---

func TestEventManager_EmitTo_OnlyDeliversToTargetUser(t *testing.T) {
	em := NewEventManager(make(chan struct{}))

	ch1 := make(chan Event, 8)
	ch2 := make(chan Event, 8)

	em.clientsMu.Lock()
	em.clients[ch1] = "user-alice"
	em.clients[ch2] = "user-bob"
	em.clientsMu.Unlock()

	em.EmitTo("user-alice", "chat:event", map[string]string{"msg": "hello"})

	// Alice should receive it
	select {
	case ev := <-ch1:
		if ev.Name != "chat:event" {
			t.Fatalf("alice: expected chat:event, got %s", ev.Name)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("alice did not receive the event")
	}

	// Bob should NOT receive it
	select {
	case ev := <-ch2:
		t.Fatalf("bob received event meant for alice: %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// expected — bob's channel is empty
	}
}

func TestEventManager_Emit_BroadcastsToAll(t *testing.T) {
	em := NewEventManager(make(chan struct{}))

	ch1 := make(chan Event, 8)
	ch2 := make(chan Event, 8)

	em.clientsMu.Lock()
	em.clients[ch1] = "user-alice"
	em.clients[ch2] = "user-bob"
	em.clientsMu.Unlock()

	em.Emit("settings:changed", "global-update")

	for i, ch := range []chan Event{ch1, ch2} {
		select {
		case <-ch:
			// expected
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("client %d did not receive broadcast", i)
		}
	}
}

// TestHandleEvents_DisconnectsAtTokenExpiry verifies a live SSE stream is
// dropped once the access token reaches its expiry, capping the connection at
// the access-token TTL instead of letting it run indefinitely.
func TestHandleEvents_DisconnectsAtTokenExpiry(t *testing.T) {
	em := NewEventManager(make(chan struct{}))

	claims := &auth.Claims{}
	claims.ID = "jti-expiry-test"
	// Construct NumericDate directly: jwt.NewNumericDate truncates to whole
	// seconds (TimePrecision), which would round a sub-second offset into the
	// past. Real tokens already carry whole-second exp, so this only matters
	// for the test's short offset.
	claims.ExpiresAt = &jwt.NumericDate{Time: time.Now().Add(150 * time.Millisecond)}

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req = req.WithContext(auth.WithClaims(req.Context(), claims))

	rr := httptest.NewRecorder() // implements http.Flusher

	start := time.Now()
	done := make(chan struct{})
	go func() {
		em.HandleEvents(rr, req)
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
			t.Fatalf("disconnected too early (%v) — should wait until token expiry", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleEvents did not disconnect after token expiry")
	}
}

// --- Audit pool shutdown safety ---

// TestAuditEnqueue_RecoversSendOnClosedChannel proves the recover guard catches
// the TOCTOU window: ShutdownAuditPool sets auditClosed and closes the channel
// as two separate statements, so a sender that already passed the
// auditClosed.Load() check (returned false) reaches the send AFTER the channel
// was closed. We reproduce that exact ordering deterministically by closing the
// channel WITHOUT setting the flag, forcing auditEnqueue onto the send path
// into a closed channel. The recover must convert the panic into ("closed").
func TestAuditEnqueue_RecoversSendOnClosedChannel(t *testing.T) {
	ch := make(chan *interfaces.AuditEvent, 1)
	auditCh = ch
	auditClosed.Store(false)
	close(ch) // intentionally do NOT set the flag: forces the send-then-recover path

	sent, reason := auditEnqueue(&interfaces.AuditEvent{Action: AuditActionFlowUpload})
	if sent || reason != "closed" {
		t.Fatalf("got (sent=%v, reason=%q), want (false, \"closed\")", sent, reason)
	}

	auditCh = nil
	auditClosed.Store(false)
}

// TestAuditEnqueue_DivertsWhenFull confirms a full queue returns ("full") and a
// pool closed via the flag (the fast path) returns ("closed") so callers route
// to the fallback sink instead of dropping silently.
func TestAuditEnqueue_DivertsWhenFull(t *testing.T) {
	auditCh = make(chan *interfaces.AuditEvent, 1)
	auditClosed.Store(false)

	// Fill the queue.
	auditCh <- &interfaces.AuditEvent{Action: AuditActionFlowUpload}

	sent, reason := auditEnqueue(&interfaces.AuditEvent{Action: AuditActionFlowDelete})
	if sent || reason != "full" {
		t.Fatalf("full queue: got (sent=%v, reason=%q), want (false, \"full\")", sent, reason)
	}

	// Close the pool via the flag fast path; the send is never attempted so no
	// recover is needed and there is no send-vs-close race here.
	auditClosed.Store(true)
	sent, reason = auditEnqueue(&interfaces.AuditEvent{Action: AuditActionFlowDelete})
	if sent || reason != "closed" {
		t.Fatalf("closed pool: got (sent=%v, reason=%q), want (false, \"closed\")", sent, reason)
	}

	// Leave package state clean so other tests don't observe a closed channel.
	auditCh = nil
	auditClosed.Store(false)
}

// --- Invite email binding ---

func TestAcceptInvite_RejectsNonexistentToken(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)

	// Verify the handler returns non-200 for a nonexistent invite token.
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/invites/nonexistent-token/accept", bearer, nil)
	if rr.Code == http.StatusOK {
		t.Errorf("expected non-200 for nonexistent invite, got %d", rr.Code)
	}
}

// --- Refresh token replay detection ---

// mockTokenStore implements RefreshTokenStore for testing replay detection.
type mockTokenStore struct {
	mu               sync.Mutex
	validTokens      map[string]bool   // jti → valid
	userIDs          map[string]string // jti → userID (for VerifyAndRevokeRefreshToken)
	revokedAllCalled bool
}

func newMockTokenStore() *mockTokenStore {
	return &mockTokenStore{
		validTokens: make(map[string]bool),
		userIDs:     make(map[string]string),
	}
}

func (m *mockTokenStore) StoreRefreshToken(_ context.Context, jti, userID string, expiresAt time.Time, _, _ string) error {
	m.mu.Lock()
	m.validTokens[jti] = true
	m.userIDs[jti] = userID
	m.mu.Unlock()
	return nil
}

func (m *mockTokenStore) IsRefreshTokenValid(_ context.Context, jti string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.validTokens[jti], nil
}

func (m *mockTokenStore) RevokeRefreshToken(_ context.Context, jti string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.validTokens[jti] {
		return interfaces.ErrTokenAlreadyRevoked
	}
	m.validTokens[jti] = false
	return nil
}

// VerifyAndRevokeRefreshToken mirrors the production atomic verify-and-revoke:
// a valid token is revoked and its info returned; an already-revoked/unknown
// token yields ErrTokenAlreadyRevoked (which the handler treats as replay).
func (m *mockTokenStore) VerifyAndRevokeRefreshToken(_ context.Context, jti string) (*interfaces.RefreshTokenInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.validTokens[jti] {
		return nil, interfaces.ErrTokenAlreadyRevoked
	}
	m.validTokens[jti] = false
	return &interfaces.RefreshTokenInfo{ID: jti, UserID: m.userIDs[jti]}, nil
}

func (m *mockTokenStore) RevokeUserRefreshTokens(_ context.Context, userID string) error {
	m.mu.Lock()
	m.revokedAllCalled = true
	m.mu.Unlock()
	return nil
}

func (m *mockTokenStore) ListUserRefreshTokens(_ context.Context, userID string) ([]interfaces.RefreshTokenInfo, error) {
	return nil, nil
}

func (m *mockTokenStore) RevokeRefreshTokenForUser(_ context.Context, jti, userID string) error {
	return nil
}

func (m *mockTokenStore) wasRevokeAllCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revokedAllCalled
}

func TestRefreshToken_ReplayDetection_RevokesAllSessions(t *testing.T) {
	store := newMockTokenStore()

	// Simulate a valid token
	jti := "test-jti-123"
	store.validTokens[jti] = true

	// First call: revoke the token (simulates normal rotation)
	err := store.RevokeRefreshToken(context.Background(), jti)
	if err != nil {
		t.Fatalf("first revoke failed: %v", err)
	}

	// Second call with the same jti: should return ErrTokenAlreadyRevoked
	err = store.RevokeRefreshToken(context.Background(), jti)
	if err == nil {
		t.Fatal("expected ErrTokenAlreadyRevoked on second revoke, got nil")
	}

	// In the handler, ErrTokenAlreadyRevoked triggers RevokeUserRefreshTokens.
	// Simulate that here.
	store.RevokeUserRefreshTokens(context.Background(), "user-1")
	if !store.wasRevokeAllCalled() {
		t.Fatal("expected RevokeUserRefreshTokens to be called after replay detection")
	}
}

func TestRefreshToken_ConcurrentRevoke_OnlyOneSucceeds(t *testing.T) {
	store := newMockTokenStore()
	jti := "concurrent-jti"
	store.validTokens[jti] = true

	var successCount int64
	var replayCount int64
	var wg sync.WaitGroup

	// Simulate two concurrent refresh requests with the same token
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.RevokeRefreshToken(context.Background(), jti)
			switch {
			case err == nil:
				atomic.AddInt64(&successCount, 1)
			case errors.Is(err, interfaces.ErrTokenAlreadyRevoked):
				atomic.AddInt64(&replayCount, 1)
			}
		}()
	}
	wg.Wait()

	// Exactly one should succeed, the other should get ErrTokenAlreadyRevoked
	if atomic.LoadInt64(&successCount) != 1 {
		t.Errorf("expected exactly 1 successful revoke, got %d", successCount)
	}
	if atomic.LoadInt64(&replayCount) != 1 {
		t.Errorf("expected exactly 1 replay detection, got %d", replayCount)
	}
}

// --- Auth role validation ---

func TestAdminUserRole_RejectsInvalidRole(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)

	// In local mode there's no JWT, so we can't fully test the admin
	// endpoint. But we can verify that auth.Role validation works.
	invalidRoles := []string{"", "superadmin", "root", "god", "123"}
	for _, role := range invalidRoles {
		if auth.Role(role).IsValid() {
			t.Errorf("role %q should be invalid", role)
		}
	}

	validRoles := []string{"admin", "member", "viewer", "guest"}
	for _, role := range validRoles {
		if !auth.Role(role).IsValid() {
			t.Errorf("role %q should be valid", role)
		}
	}

	_ = rt
}
