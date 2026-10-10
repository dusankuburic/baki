package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

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
	claims, err := rt.security.AuthMgr.VerifyWSTicket(resp.Ticket)
	if err != nil {
		t.Fatalf("issued ticket failed to verify: %v", err)
	}
	if claims.UserID != "user-1" {
		t.Errorf("ticket UserID: got %q want %q", claims.UserID, "user-1")
	}
	// The response says when the ticket stops being accepted (additive; the client type always declared it).
	exp, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt %q is not RFC 3339: %v", resp.ExpiresAt, err)
	}
	if !exp.Equal(claims.ExpiresAt.Time) && exp.Sub(claims.ExpiresAt.Time).Abs() > time.Second {
		t.Errorf("expiresAt %v, ticket exp %v", exp, claims.ExpiresAt.Time)
	}
}

// A PAT's ticket carries the token's scopes and its revocable JTI, so the socket acts as that token (R7).
func TestWSTicket_PATTicketCarriesScopes(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "u-owner", "u-owner@acme.io", auth.RoleMember)
	session := jwtBearer(t, rt, "u-owner", "u-owner@acme.io")
	pat := mintPAT(t, rt, session, []string{auth.ScopeRead})

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/ws-ticket", pat, nil)
	checkStatus(t, rr, http.StatusOK)
	var resp struct {
		Ticket string `json:"ticket"`
	}
	decodeJSON(t, rr, &resp)
	claims, err := rt.security.AuthMgr.VerifyWSTicket(resp.Ticket)
	if err != nil {
		t.Fatalf("issued ticket failed to verify: %v", err)
	}
	actor := auth.ActorFromWSTicket(claims)
	if actor.Kind != auth.ActorToken || actor.TokenID == "" || !slices.Equal(actor.Scopes, []string{auth.ScopeRead}) {
		t.Fatalf("a read PAT's ticket stands for %+v, want the read-scoped token", actor)
	}
	if claims.SrcJTI != auth.PATJTI(actor.TokenID) {
		t.Errorf("ticket src_jti %q, want the PAT's revocable jti", claims.SrcJTI)
	}
}

// Regression: PAT claims used to come with a zero-valued RegisteredClaims, and
// a PAT without an expiry still has a nil ExpiresAt (ClaimsForPAT). handleWSTicket
// used to dereference claims.ExpiresAt.Time unconditionally, panicking into a
// 500 on every such ws-ticket request (and a stack-dump DoS on repeat). Such
// claims must not panic and must yield a usable ticket.
func TestWSTicket_PATClaimsWithNilExpiresAt_DoesNotPanic(t *testing.T) {
	rt := newJWTTestRouter(t)
	// Reuse the router's configured AuthMgr; handleWSTicket only needs security.
	h := NewAuthHandler(nil, nil, rt.security, nil, nil, nil, nil, nil, nil, nil)

	// PAT-style claims: exactly what verifyAPIToken returns — ExpiresAt is nil.
	patClaims := &auth.Claims{UserID: "ci-bot", Email: "bot@example.com", Role: auth.RoleAdmin}
	if patClaims.ExpiresAt != nil {
		t.Fatalf("test fixture: expected nil ExpiresAt for PAT-style claims")
	}

	r := httptest.NewRequest(http.MethodPost, "/api/ws-ticket", nil)
	r = r.WithContext(auth.WithClaims(r.Context(), patClaims))
	rr := httptest.NewRecorder()

	h.handleWSTicket(rr, r) // must not panic
	checkStatus(t, rr, http.StatusOK)

	var resp struct {
		Ticket string `json:"ticket"`
	}
	decodeJSON(t, rr, &resp)
	if resp.Ticket == "" {
		t.Fatal("expected a ticket for a PAT-authenticated request")
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

// A ticket without the version claim comes from a server that predates ticket
// scopes: its missing scopes cannot be read as "unscoped", so /ws refuses it
// (401) and the client reconnects with a fresh ticket.
func TestWS_RejectsVersionlessTicket(t *testing.T) {
	rt := newJWTTestRouter(t)
	now := time.Now()
	legacy := auth.Claims{UserID: "user-1", Email: "alice@example.com", Role: auth.RoleMember, SrcJTI: auth.PATJTI("tok-1")}
	legacy.RegisteredClaims = jwt.RegisteredClaims{
		ID: "legacy-ticket", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(30 * time.Second)),
		Subject: "user-1", Issuer: "pad-analyzer", Audience: jwt.ClaimStrings{"pad-ws-ticket"},
	}
	ticket, err := jwt.NewWithClaims(jwt.SigningMethodHS256, legacy).SignedString([]byte(testToken))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/ws?flowId=f1&ticket="+ticket, nil)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	checkStatus(t, rr, http.StatusUnauthorized)
}

// An access token presented in the ?ticket= slot must be rejected (audience
// separation): it would otherwise let the long-lived token authorize the WS.
func TestWS_RejectsAccessTokenAsTicket(t *testing.T) {
	rt := newJWTTestRouter(t)
	pair, _ := rt.security.AuthMgr.Issue("user-1", "alice@example.com", auth.RoleAdmin)

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
	ticket, _, err := rt.security.AuthMgr.IssueWSTicket(auth.WSTicketSource{UserID: "user-1", Email: "alice@example.com", Role: auth.RoleAdmin})
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
