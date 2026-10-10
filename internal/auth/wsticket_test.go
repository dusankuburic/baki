package auth

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// A ticket carries the source credential's scopes, as a copy, and the format version; an unscoped source gives a
// ticket without scopes.
func TestIssueWSTicket_CarriesScopesAndVersion(t *testing.T) {
	mgr := newTestManager()
	scopes := []string{ScopeRead}
	ticket, _, err := mgr.IssueWSTicket(WSTicketSource{UserID: "u1", Role: RoleMember, Scopes: scopes, AccessJTI: PATJTI("tok1")})
	if err != nil {
		t.Fatal(err)
	}
	scopes[0] = ScopeAdmin
	claims, err := mgr.VerifyWSTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if claims.WSV != WSTicketVersion || !slices.Equal(claims.Scopes, []string{ScopeRead}) || claims.SrcJTI != "pat:tok1" {
		t.Fatalf("ticket claims = wsv %d scopes %v src_jti %q", claims.WSV, claims.Scopes, claims.SrcJTI)
	}

	ticket, _, err = mgr.IssueWSTicket(WSTicketSource{UserID: "u1", Role: RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if claims, err = mgr.VerifyWSTicket(ticket); err != nil || claims.Scopes != nil || claims.WSV != WSTicketVersion {
		t.Fatalf("unscoped ticket: claims %+v, err %v", claims, err)
	}
}

// signLegacyTicket signs a ticket as a server before the version claim did: valid audience, issuer and expiry, no
// wsv and no scopes.
func signLegacyTicket(t *testing.T, mgr *Manager) string {
	t.Helper()
	now := time.Now()
	claims := Claims{UserID: "u1", Email: "u1@x.test", Role: RoleMember, SrcJTI: PATJTI("tok1")}
	claims.RegisteredClaims = jwt.RegisteredClaims{
		ID:        uuid.NewString(),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(wsTicketTTL)),
		Subject:   "u1",
		Issuer:    mgr.issuer,
		Audience:  jwt.ClaimStrings{wsTicketAudience},
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(mgr.secret)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A versionless ticket comes from an older server and cannot say whether its credential was scoped: reading the
// missing scopes as unscoped would widen a read-only token, so it is refused, and the client fetches a new one.
// The refusal happens before the single-use mark.
func TestConsumeWSTicket_RefusesVersionlessTicket(t *testing.T) {
	bl := NewTokenBlacklist()
	mgr := NewManagerWithTTL(testSecret, 2*time.Second, 5*time.Second, "test-issuer", "test-audience", bl)
	legacy := signLegacyTicket(t, mgr)
	if _, err := mgr.ConsumeWSTicket(legacy); err == nil || !strings.Contains(err.Error(), "older server") {
		t.Fatalf("versionless ticket consumed (err %v)", err)
	}
	if _, err := mgr.VerifyWSTicket(legacy); err == nil {
		t.Fatal("versionless ticket verified")
	}

	ticket, _, err := mgr.IssueWSTicket(WSTicketSource{UserID: "u1", Role: RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ConsumeWSTicket(ticket); err != nil {
		t.Fatalf("current ticket refused: %v", err)
	}
	if _, err := mgr.ConsumeWSTicket(ticket); err == nil {
		t.Fatal("current ticket consumed twice")
	}
}

// The socket's actor is the credential the ticket was issued from, read through src_jti: a session's ticket is the
// user, a PAT's is that token (with its id), and a scoped PAT's carries the scopes.
func TestActorFromWSTicket(t *testing.T) {
	mgr := newTestManager()
	cases := []struct {
		name      string
		src       WSTicketSource
		wantKind  ActorKind
		wantToken string
		wantScope []string
	}{
		{"session", WSTicketSource{AccessJTI: "jwt-jti-1"}, ActorUser, "", nil},
		{"local mode", WSTicketSource{}, ActorUser, "", nil},
		{"unscoped PAT", WSTicketSource{AccessJTI: PATJTI("tok1")}, ActorToken, "tok1", nil},
		{"scoped PAT", WSTicketSource{AccessJTI: PATJTI("tok1"), Scopes: []string{ScopeRead}}, ActorToken, "tok1", []string{ScopeRead}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.src.UserID, tc.src.Email, tc.src.Role = "u1", "u1@x.test", RoleMember
			ticket, _, err := mgr.IssueWSTicket(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := mgr.VerifyWSTicket(ticket)
			if err != nil {
				t.Fatal(err)
			}
			a := ActorFromWSTicket(claims)
			if a.Kind != tc.wantKind || a.TokenID != tc.wantToken || !slices.Equal(a.Scopes, tc.wantScope) {
				t.Fatalf("got kind=%v token=%q scopes=%v, want kind=%v token=%q scopes=%v",
					a.Kind, a.TokenID, a.Scopes, tc.wantKind, tc.wantToken, tc.wantScope)
			}
			if a.UserID != "u1" || a.Email != "u1@x.test" || a.Role != RoleMember {
				t.Fatalf("identity not carried over: %+v", a)
			}
			if claims.ID == tc.src.AccessJTI || claims.SrcJTI != tc.src.AccessJTI {
				t.Fatalf("ActorFromWSTicket changed the claims: id %q src_jti %q", claims.ID, claims.SrcJTI)
			}
		})
	}
	if ActorFromWSTicket(nil) != nil {
		t.Fatal("nil claims must give a nil actor")
	}
}
