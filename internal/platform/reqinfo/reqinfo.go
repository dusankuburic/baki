// Package reqinfo is what the HTTP layers learn about a request, shared by pointer from the outermost layer
// inward: the request id, the client and peer addresses, the user agent, the resolved route policy, and the
// actor, which authentication records once it knows who is calling. Outer layers (the access log, metrics) read
// what inner ones (jwtAuth, login, SSO, the WebSocket upgrade) recorded, which a context value set further in
// cannot give them.
package reqinfo

import (
	"context"
	"sync/atomic"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
)

// Info is one request's shared record. It is created once per request and never reused or pooled.
type Info struct {
	// ID is the X-Request-ID the request arrived with, or a fresh UUID.
	ID string
	// ClientIP is the proxy-aware client address (the trusted-proxy rules of middleware.ClientIP).
	ClientIP string
	// PeerIP is the TCP peer's address, whatever the headers claim.
	PeerIP    string
	UserAgent string
	// Policy is the route policy resolved at the outermost layer; nil until then.
	Policy *route.Policy

	actor atomic.Pointer[auth.Actor]
}

type ctxKey struct{}

// With returns ctx carrying info.
func With(ctx context.Context, info *Info) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

// From returns the request's Info, or nil outside an HTTP request.
func From(ctx context.Context) *Info {
	info, _ := ctx.Value(ctxKey{}).(*Info)
	return info
}

// SetActor records who the request acts as. The first actor recorded stays: a later call returns false and
// changes nothing, so no layer can re-identify a request another layer has authenticated.
func (i *Info) SetActor(a *auth.Actor) bool {
	if i == nil || a == nil {
		return false
	}
	return i.actor.CompareAndSwap(nil, a)
}

// Actor returns the recorded actor, or nil.
func (i *Info) Actor() *auth.Actor {
	if i == nil {
		return nil
	}
	return i.actor.Load()
}
