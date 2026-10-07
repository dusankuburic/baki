package api

import (
	"net/http"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/reqinfo"
)

// recordActor records who r acts as on the request's reqinfo.Info, where the layers outside the handler read it:
// the access log's user_id and logger.FromContext lines. The first actor recorded stays (reqinfo.Info.SetActor).
// Without an Info, as in tests that call the Router directly, nothing is recorded.
func recordActor(r *http.Request, a *auth.Actor) {
	reqinfo.From(r.Context()).SetActor(a)
}

// recordUser records a user the request has just authenticated itself, before any token exists: login,
// registration that signs in, refresh, the SSO sign-in.
func recordUser(r *http.Request, userID, email string, role auth.Role) {
	recordActor(r, &auth.Actor{Kind: auth.ActorUser, UserID: userID, Email: email, Role: role})
}

// recordCaller is jwtAuth's record of a request it let through as authenticated: the user or API token of the
// claims it put on the context, or, in local mode, which has no claims, the single local user it installed
// (auth.LocalActor).
func (c *SecurityConfig) recordCaller(r *http.Request) {
	recordActor(r, auth.ActorFromContext(r.Context()))
}
