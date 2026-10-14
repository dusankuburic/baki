package api

import (
	"testing"

	"pad-analyzer/internal/platform/eventbus"
)

// A peer's revocation nudge (recheck.sse-revocation) ends this replica's streams of the token, as the local
// revocation does; the peer did its own. A scope without a token id ends nothing: Revoke("") would end every stream
// opened without a credential, and an empty scope (the receiver's resync after a gap) has no token to name, so the
// periodic blacklist check stays the backstop for a revocation a lost message hid.
func TestEvents_AppliesAPeersRevocation(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	revoked, stopRevoked := m.watchRevocation("session-1")
	defer stopRevoked()
	other, stopOther := m.watchRevocation("session-2")
	defer stopOther()
	anonymous, stopAnonymous := m.watchRevocation("")
	defer stopAnonymous()
	closed := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}

	m.ApplyRemoteRevocation(nil)
	m.ApplyRemoteRevocation(eventbus.NewScope("jti", ""))
	m.ApplyRemoteRevocation(eventbus.NewScope("what", "x"))
	if closed(revoked) || closed(other) || closed(anonymous) {
		t.Fatal("a scope naming no token ended a stream")
	}
	m.ApplyRemoteRevocation(eventbus.NewScope("jti", "session-1"))
	if !closed(revoked) {
		t.Error("the revoked token's stream is still open")
	}
	if closed(other) || closed(anonymous) {
		t.Error("another token's stream, or the anonymous one, ended")
	}
}
