package auth

import (
	"context"
	"slices"
	"testing"
)

func TestActorFromClaims(t *testing.T) {
	cases := []struct {
		name      string
		claims    *Claims
		wantKind  ActorKind
		wantToken string
		wantScope []string
	}{
		{"session", &Claims{UserID: "u1", Email: "u1@x.test", Role: RoleMember}, ActorUser, "", nil},
		{"unscoped PAT", ClaimsForPAT("u1", "u1@x.test", RoleMember, "tok1", nil, nil), ActorToken, "tok1", nil},
		{"scoped PAT", ClaimsForPAT("u1", "u1@x.test", RoleMember, "tok1", nil, []string{ScopeRead}), ActorToken, "tok1", []string{ScopeRead}},
		// Scopes restrict whoever carries them: claims with scopes but no PAT id must not turn into a session.
		{"scopes without a PAT id", &Claims{UserID: "u1", Role: RoleMember, Scopes: []string{ScopeChat}}, ActorToken, "", []string{ScopeChat}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := ActorFromClaims(tc.claims)
			if a.Kind != tc.wantKind || a.TokenID != tc.wantToken || !slices.Equal(a.Scopes, tc.wantScope) {
				t.Fatalf("got kind=%v token=%q scopes=%v, want kind=%v token=%q scopes=%v",
					a.Kind, a.TokenID, a.Scopes, tc.wantKind, tc.wantToken, tc.wantScope)
			}
			if a.UserID != "u1" || a.Role != RoleMember {
				t.Fatalf("identity not carried over: %+v", a)
			}
		})
	}
	if ActorFromClaims(nil) != nil {
		t.Fatal("nil claims must give a nil actor")
	}
}

// The actor owns its scopes: changing the claims afterwards must not widen or narrow it.
func TestActorFromClaims_CopiesScopes(t *testing.T) {
	c := ClaimsForPAT("u1", "", RoleMember, "tok1", nil, []string{ScopeRead})
	a := ActorFromClaims(c)
	c.Scopes[0] = ScopeAdmin
	if a.Allows(ScopeAdmin) || !a.Allows(ScopeRead) {
		t.Fatalf("actor scopes alias the claims: %v", a.Scopes)
	}
}

func TestActorAllows(t *testing.T) {
	var none *Actor
	session := &Actor{Kind: ActorUser, UserID: "u1"}
	scoped := &Actor{Kind: ActorToken, UserID: "u1", Scopes: []string{ScopeRead, ScopeChat}}
	for _, s := range ValidTokenScopes {
		if none.Allows(s) {
			t.Errorf("nil actor allows %q", s)
		}
		if !session.Allows(s) || !SystemActor("job").Allows(s) || !LocalActor("local").Allows(s) {
			t.Errorf("an unscoped actor refuses %q", s)
		}
		if want := s == ScopeRead || s == ScopeChat; scoped.Allows(s) != want {
			t.Errorf("scoped token Allows(%q) = %v, want %v", s, !want, want)
		}
	}
	if none.Scoped() || session.Scoped() || !scoped.Scoped() {
		t.Error("Scoped must hold for the scope-restricted token only")
	}
}

func TestSystemAndLocalActors(t *testing.T) {
	sys := SystemActor("retention")
	if sys.Kind != ActorSystem || sys.Job != "retention" || sys.UserID != "" {
		t.Errorf("SystemActor = %+v", sys)
	}
	// Local mode's single user passes the global role gates, as RequireRole always let local mode through.
	local := LocalActor("local-user")
	if local.Kind != ActorUser || local.UserID != "local-user" || local.Role != RoleAdmin || local.Scoped() {
		t.Errorf("LocalActor = %+v", local)
	}
	for k, want := range map[ActorKind]string{ActorUser: "user", ActorToken: "token", ActorSystem: "system", 0: "unknown"} {
		if k.String() != want {
			t.Errorf("ActorKind(%d).String() = %q, want %q", k, k.String(), want)
		}
	}
}

func TestActorFromContext(t *testing.T) {
	bg := context.Background()
	if ActorFromContext(bg) != nil {
		t.Fatal("a context without claims or an actor must give nil")
	}

	withClaims := WithClaims(bg, ClaimsForPAT("u1", "", RoleMember, "tok1", nil, []string{ScopeChat}))
	a := ActorFromContext(withClaims)
	if a == nil || a.Kind != ActorToken || a.UserID != "u1" || !a.Allows(ScopeChat) || a.Allows(ScopeWrite) {
		t.Fatalf("claims fallback = %+v", a)
	}

	// An explicit actor wins over the request's claims: a background job derived from a request runs as itself.
	sys := SystemActor("scanner")
	if got := ActorFromContext(WithActor(withClaims, sys)); got != sys {
		t.Fatalf("explicit actor lost to the claims: %+v", got)
	}
	if got := ActorFromContext(WithActor(bg, sys)); got != sys {
		t.Fatalf("explicit actor without claims = %+v", got)
	}

	// A nil actor sets nothing: the claims still decide.
	if got := ActorFromContext(WithActor(withClaims, nil)); got == nil || got.UserID != "u1" {
		t.Fatalf("WithActor(nil) changed the actor: %+v", got)
	}
}
