package api

import (
	"context"
	"time"

	"pad-core/logger"

	"pad-analyzer/internal/redisx"
)

// wsTicketKey is the single-use record of a WebSocket ticket, by the ticket's jti (the id the token blacklist
// already publishes on baki:auth:revoked). Tickets are 30 s JWTs; the key lives as long as the ticket can still be
// presented, plus a margin. The Postgres-backed blacklist (AuthMgr.ConsumeWSTicket) stays the authority across
// replicas and fails closed; this key is the faster guard in front of it, so it fails open: a Redis error leaves the
// in-memory guard and the blacklist.
var wsTicketKey = redisx.Declare(redisx.Spec{
	Name:    "ws.ticket",
	Kind:    redisx.KindString,
	Pattern: "baki:ws:ticket:{jti}",
	TTL:     2 * time.Minute,
	OnDown:  redisx.OnDownOpen,
	Owner:   "internal/api",
})

const (
	// wsTicketMargin keeps the key past the ticket's expiry, for a clock a little behind.
	wsTicketMargin = 5 * time.Second
	// wsTicketRedisWait bounds the Redis call: a socket upgrade does not wait on a slow Redis.
	wsTicketRedisWait = 500 * time.Millisecond
)

// consumeTicketEverywhere records a single-use ticket and reports whether this is its first use: through Redis
// when it is configured (every replica sees it), then in this replica's memory (consumeTicket), which is also all
// there is when Redis is not configured or does not answer.
func (rt *Router) consumeTicketEverywhere(ctx context.Context, jti string, exp time.Time) bool {
	if jti == "" {
		return false
	}
	if rt.ticketRedis != nil {
		ttl := min(max(time.Until(exp)+wsTicketMargin, wsTicketMargin), wsTicketKey.Spec().TTL)
		rctx, cancel := context.WithTimeout(ctx, wsTicketRedisWait)
		first, err := rt.ticketRedis.SetNX(rctx, wsTicketKey.Key(jti), "1", ttl).Result()
		cancel()
		switch {
		case err != nil:
			logger.Debug("ws ticket guard unavailable; the in-memory guard and the blacklist remain")
		case !first:
			return false
		}
	}
	return rt.consumeTicket(jti, exp)
}
