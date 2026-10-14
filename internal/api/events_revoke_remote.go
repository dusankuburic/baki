package api

import "pad-analyzer/internal/platform/eventbus"

// ApplyRemoteRevocation is the applier of recheck.sse-revocation (Bus.OnRemote): a peer revoked an access token (a
// logout, a deleted API token), and this replica ends its streams opened with it, as Revoke does for a local
// revocation. The scope is {"jti": ...} as SubscribeRevocations writes it. Nothing without a token id: Revoke("")
// would end every stream opened without a credential, and a resync (empty scope) names no token, so the periodic
// blacklist check remains the backstop for a revocation whose message was lost.
func (m *EventManager) ApplyRemoteRevocation(scope eventbus.Scope) {
	if jti := scope["jti"]; jti != "" {
		m.Revoke(jti)
	}
}

// SubscribeRemoteRevocations registers ApplyRemoteRevocation as the applier of recheck.sse-revocation, the key
// SubscribeRevocations publishes.
func (m *EventManager) SubscribeRemoteRevocations(bus *eventbus.Bus) {
	bus.OnRemote("recheck.sse-revocation", m.ApplyRemoteRevocation)
}
