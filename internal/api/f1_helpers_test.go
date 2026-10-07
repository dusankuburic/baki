package api

import (
	"context"

	"pad-analyzer/internal/auth"
)

// userActorCtx is a context acting as userID's interactive session (a member), the way jwtAuth installs a session's
// actor: for calling the services' authorized reads directly from a test.
func userActorCtx(userID string) context.Context {
	return auth.WithActor(context.Background(), &auth.Actor{Kind: auth.ActorUser, UserID: userID, Role: auth.RoleMember})
}
