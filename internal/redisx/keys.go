package redisx

// The families whose owner cannot declare them itself. internal/auth stays free of the Redis dependency (it
// defines the publisher and subscriber interfaces this package implements), so its channel is declared here and
// auth keeps the name as a constant of its own; TestRedisKeys_AuthConstantMatches pins the two together.
// Every other family is declared by the package that owns it, beside the code that reads and writes it.

// RevocationChannel carries token revocations to the peer replicas (auth.CachedBlacklist). The message is
// {"jti","expires_at"} (v0): the durable source is Postgres, a replica that misses a message reconciles at its next
// 30 s reload, and a read it cannot answer from the local set goes to Postgres and fails closed.
var RevocationChannel = Declare(Spec{
	Name:    "auth.revoked",
	Kind:    KindPubSub,
	Pattern: "baki:auth:revoked",
	OnDown:  OnDownClosed,
	Owner:   "internal/auth",
})
