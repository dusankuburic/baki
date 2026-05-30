package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
)

// POST /api/ws-ticket with a valid access token returns a usable ticket.
func TestWSTicket_IssuedForAuthenticatedUser(t *testing.T) {
	rt := newJWTTestRouter(t)
	bearer := jwtBearer(t, rt, "user-1", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/ws-ticket", bearer, nil)
	checkStatus(t, rr, http.StatusOK)

	var resp struct {
		Ticket    string `json:"ticket"`
		ExpiresAt string `json:"expiresAt"`
	}
	decodeJSON(t, rr, &resp)
	if resp.Ticket == "" {
		t.Fatal("expected a non-empty ticket")
	}
	// The ticket must verify and carry the requesting user's identity.
	claims, err := rt.authMgr.VerifyWSTicket(resp.Ticket)
	if err != nil {
		t.Fatalf("issued ticket failed to verify: %v", err)
	}
	if claims.UserID != "user-1" {
		t.Errorf("ticket UserID: got %q want %q", claims.UserID, "user-1")
	}
}

// The ticket endpoint requires authentication in cloud mode.
func TestWSTicket_RequiresAuth(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/ws-ticket", "", nil)
	checkStatus(t, rr, http.StatusUnauthorized)
}

// /ws without a ticket, or with a bogus ticket, is rejected before any upgrade.
func TestWS_RejectsMissingOrInvalidTicket(t *testing.T) {
	rt := newJWTTestRouter(t)

	for _, path := range []string{"/ws?flowId=f1", "/ws?flowId=f1&ticket=garbage"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		rt.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %d", path, rr.Code)
		}
	}
}

// An access token presented in the ?ticket= slot must be rejected (audience
// separation): it would otherwise let the long-lived token authorize the WS.
func TestWS_RejectsAccessTokenAsTicket(t *testing.T) {
	rt := newJWTTestRouter(t)
	pair, _ := rt.authMgr.Issue("user-1", "alice@example.com", auth.RoleAdmin)

	req := httptest.NewRequest(http.MethodGet, "/ws?flowId=f1&ticket="+pair.AccessToken, nil)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for access token used as ticket, got %d", rr.Code)
	}
}

// A ticket can be redeemed at most once: the second attempt with the same
// ticket is rejected as a replay. (The first attempt passes auth and only fails
// later at the WebSocket upgrade, which httptest cannot hijack — so we assert it
// is NOT a 401, while the replay IS a 401.)
func TestWS_TicketIsSingleUse(t *testing.T) {
	rt := newJWTTestRouter(t)
	ticket, _, err := rt.authMgr.IssueWSTicket("user-1", "alice@example.com", auth.RoleAdmin)
	if err != nil {
		t.Fatalf("IssueWSTicket: %v", err)
	}

	do := func() int {
		req := httptest.NewRequest(http.MethodGet, "/ws?flowId=f1&ticket="+ticket, nil)
		rr := httptest.NewRecorder()
		rt.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := do(); code == http.StatusUnauthorized {
		t.Fatalf("first redemption should pass auth, got 401")
	}
	if code := do(); code != http.StatusUnauthorized {
		t.Fatalf("second redemption (replay) should be 401, got %d", code)
	}
}

// consumeTicket enforces single use and prunes expired entries.
func TestConsumeTicket(t *testing.T) {
	rt := newJWTTestRouter(t)
	future := time.Now().Add(time.Minute)

	if !rt.consumeTicket("jti-1", future) {
		t.Fatal("first consume should succeed")
	}
	if rt.consumeTicket("jti-1", future) {
		t.Fatal("second consume of same jti should fail (replay)")
	}
	if rt.consumeTicket("", future) {
		t.Fatal("empty jti should be rejected")
	}

	// An already-expired entry is pruned, so the jti can be (re)inserted; but a
	// freshly inserted not-yet-expired jti must still block replay.
	if !rt.consumeTicket("jti-2", time.Now().Add(-time.Second)) {
		t.Fatal("inserting a new jti should succeed even if its exp is in the past")
	}
}

// TestConsumeTicket_CapEnforced verifies the bounded-store guard: once the
// map hits maxUsedTickets of unexpired entries, further inserts are refused
// rather than evicting arbitrary entries (which would allow replay).
func TestConsumeTicket_CapEnforced(t *testing.T) {
	rt := newJWTTestRouter(t)
	future := time.Now().Add(time.Hour)

	// Fill the map to the cap with unique, far-future jtis.
	for i := range maxUsedTickets {
		jti := fmt.Sprintf("cap-%d", i)
		if !rt.consumeTicket(jti, future) {
			t.Fatalf("expected consume %d to succeed under the cap", i)
		}
	}
	// The next insert must be refused because no entries are expired and
	// the cap is reached.
	if rt.consumeTicket("over-cap", future) {
		t.Error("consume past the cap should be refused (no eviction allowed)")
	}
	// Insert an expired entry by directly placing one in the past, then
	// the next call should prune it and accept a new jti.
	rt.usedTicketsMu.Lock()
	rt.usedTickets["cap-0"] = time.Now().Add(-time.Second)
	rt.usedTicketsMu.Unlock()
	if !rt.consumeTicket("after-prune", future) {
		t.Error("consume after pruning an expired entry should succeed")
	}
}
