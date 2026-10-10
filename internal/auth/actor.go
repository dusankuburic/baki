package auth

import (
	"context"
	"strings"
)

// ActorKind tells the kinds of Actor apart.
type ActorKind uint8

const (
	// ActorUser: a signed-in user (an interactive session, or local mode's single user).
	ActorUser ActorKind = iota + 1
	// ActorToken: a personal access token acting for its owner, possibly restricted by scopes.
	ActorToken
	// ActorSystem: the process itself, running a background job (scanner, retention, migrations).
	ActorSystem
	// ActorIntegration: a machine credential that is no user, an organisation's CI credential or the deployment's CI
	// webhook secret. It holds no role and no scope, so authorization refuses it everything; audit records it.
	ActorIntegration
)

func (k ActorKind) String() string {
	switch k {
	case ActorUser:
		return "user"
	case ActorToken:
		return "token"
	case ActorSystem:
		return "system"
	case ActorIntegration:
		return "integration"
	}
	return "unknown"
}

// Actor is who a request or a background job acts as. Authorization decides on it (role, scopes) and audit
// records it. Never modify an Actor once it is shared.
type Actor struct {
	Kind   ActorKind
	UserID string
	Email  string
	Role   Role
	// Scopes restrict a scoped API token; empty means unrestricted (sessions, unscoped tokens, the system).
	Scopes []string
	// TokenID identifies the API token of an ActorToken.
	TokenID string
	// Job names the background job of an ActorSystem.
	Job string
	// OrgID is the organisation an ActorIntegration's credential belongs to; "" for the deployment's secret.
	OrgID string
}

// ActorFromClaims returns the actor authenticated claims stand for: an ActorToken for claims built by
// ClaimsForPAT, an ActorUser otherwise. Nil claims give a nil actor. Claims carrying scopes are a token's even
// without a PAT id: dropping a restriction the claims carry would fail open.
func ActorFromClaims(c *Claims) *Actor {
	if c == nil {
		return nil
	}
	a := &Actor{Kind: ActorUser, UserID: c.UserID, Email: c.Email, Role: c.Role}
	tokenID, isPAT := strings.CutPrefix(c.ID, PATJTI(""))
	if isPAT || len(c.Scopes) > 0 {
		a.Kind = ActorToken
		if isPAT {
			a.TokenID = tokenID
		}
		a.Scopes = append([]string(nil), c.Scopes...)
	}
	return a
}

// ActorFromWSTicket returns the actor a WebSocket ticket's claims stand for: the credential the ticket was issued
// from. That is ActorFromClaims read through the source credential's jti (SrcJTI) instead of the ticket's own, so a
// ticket issued from a PAT is the ActorToken of that PAT, with its scopes. Nil claims give a nil actor.
func ActorFromWSTicket(c *Claims) *Actor {
	if c == nil {
		return nil
	}
	src := *c
	src.ID = c.SrcJTI
	return ActorFromClaims(&src)
}

// SystemActor returns the actor a background job runs as.
func SystemActor(job string) *Actor {
	return &Actor{Kind: ActorSystem, Job: job, Role: RoleAdmin}
}

// IntegrationActor returns the actor of a CI webhook call: the organisation's CI credential credentialID, or with
// both empty the deployment's webhook secret.
func IntegrationActor(orgID, credentialID string) *Actor {
	return &Actor{Kind: ActorIntegration, OrgID: orgID, TokenID: credentialID}
}

// LocalActor returns the actor of a deployment without accounts (local mode): its single user, who holds the
// admin role because the role gates have always let local mode through.
func LocalActor(userID string) *Actor {
	return &Actor{Kind: ActorUser, UserID: userID, Role: RoleAdmin}
}

const actorKey contextKey = "auth_actor"

// WithActor returns ctx carrying an explicit actor: a background job's SystemActor, or the local user where no
// claims exist. It takes precedence over the claims in ActorFromContext; a nil actor sets nothing.
func WithActor(ctx context.Context, a *Actor) context.Context {
	if a == nil {
		return ctx
	}
	return context.WithValue(ctx, actorKey, a)
}

// ActorFromContext returns who ctx acts as: the actor set with WithActor, else the actor of the authenticated
// claims (ActorFromClaims), else nil.
func ActorFromContext(ctx context.Context) *Actor {
	if a, _ := ctx.Value(actorKey).(*Actor); a != nil {
		return a
	}
	return ActorFromClaims(ClaimsFromContext(ctx))
}

// Scoped reports whether the actor is a scope-restricted API token.
func (a *Actor) Scoped() bool { return a != nil && len(a.Scopes) > 0 }

// Allows reports whether the actor's token scopes permit scope. Actors that are not scoped tokens allow every
// scope; a nil actor and an integration allow none.
func (a *Actor) Allows(scope string) bool {
	if a == nil || a.Kind == ActorIntegration {
		return false
	}
	return !a.Scoped() || HasScope(a.Scopes, scope)
}
