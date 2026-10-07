package service

import "context"

// LocalUser is the audience of a deployment without accounts (the desktop app, auth disabled): its realtime
// clients connect with no identity, and services address them with the caller id they hold there, "". With
// accounts every client has a user id, so an event for LocalUser reaches no one; it is never a broadcast.
const LocalUser = ""

// EventNotifier sends realtime events to the frontend. Every emit names its audience: one user, the members of an
// organisation, or, explicitly, every connected client.
type EventNotifier interface {
	// EmitTo delivers to userID's clients only (LocalUser: the clients of a deployment without accounts).
	EmitTo(userID, name string, data any)
	// EmitToOrg delivers to every member of orgID, its owner included, each once. The roster is resolved where the
	// event is delivered (one read, on ctx), so a caller never assembles it.
	EmitToOrg(ctx context.Context, orgID, name string, data any)
	// EmitToAll delivers to every connected client of every user: AudienceAll, for an event that is truly global.
	// Nothing tenant-scoped may use it.
	EmitToAll(name string, data any)
	// HasSubscriber reports whether at least one client that would receive
	// EmitTo(userID, …) is currently connected. Returning false actively
	// signals "the client is gone" and lets services abandon work addressed
	// to it (e.g. cancel an orphaned chat stream), so implementations without
	// a notion of client liveness must return true.
	HasSubscriber(userID string) bool
}

// NilNotifier is a no-op implementation of EventNotifier.
type NilNotifier struct{}

func (n NilNotifier) EmitTo(userID, name string, data any)                        {}
func (n NilNotifier) EmitToOrg(ctx context.Context, orgID, name string, data any) {}
func (n NilNotifier) EmitToAll(name string, data any)                             {}
func (n NilNotifier) HasSubscriber(userID string) bool                            { return true }
