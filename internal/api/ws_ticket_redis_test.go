package api

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/testutil/redisreplica"
)

// A WebSocket ticket is single-use. The shared (Postgres) blacklist is the authority across replicas; the guard in
// front of it is per replica in memory, and with Redis configured it is shared too, so a replay through another
// replica is refused before it asks the database.

func ticketRouters(t *testing.T, n int, withRedis bool) (*redisreplica.Cluster, []*Router) {
	t.Helper()
	c := redisreplica.New(t, n)
	rts := make([]*Router, n)
	for i := range rts {
		rts[i] = &Router{usedTickets: map[string]time.Time{}}
		if withRedis {
			rts[i].ticketRedis = c.Replicas[i].Client
		}
	}
	return c, rts
}

func TestWSTicket_AReplayThroughAnotherReplicaIsRefused(t *testing.T) {
	_, rts := ticketRouters(t, 2, true)
	exp := time.Now().Add(30 * time.Second)
	ctx := context.Background()
	if !rts[0].consumeTicketEverywhere(ctx, "jti-1", exp) {
		t.Fatal("the first use of a ticket was refused")
	}
	if rts[1].consumeTicketEverywhere(ctx, "jti-1", exp) {
		t.Error("the other replica accepted a ticket already used")
	}
	if rts[0].consumeTicketEverywhere(ctx, "jti-1", exp) {
		t.Error("the same replica accepted a ticket twice")
	}
	if !rts[1].consumeTicketEverywhere(ctx, "jti-2", exp) {
		t.Error("another ticket was refused")
	}
}

func TestWSTicket_WithoutRedisTheMemoryGuardStays(t *testing.T) {
	_, rts := ticketRouters(t, 2, false)
	exp := time.Now().Add(30 * time.Second)
	ctx := context.Background()
	if !rts[0].consumeTicketEverywhere(ctx, "jti-1", exp) || rts[0].consumeTicketEverywhere(ctx, "jti-1", exp) {
		t.Error("the in-memory guard no longer admits a ticket once")
	}
	if !rts[1].consumeTicketEverywhere(ctx, "jti-1", exp) {
		t.Error("without Redis each replica guards its own (the shared blacklist is the cross-replica authority)")
	}
	if rts[0].consumeTicketEverywhere(ctx, "", exp) {
		t.Error("a ticket without an id was admitted")
	}
}

// Redis away: the guard fails open to the memory one, never refuses a first use, and does not wait.
func TestWSTicket_RedisDownFallsBackToMemory(t *testing.T) {
	c, rts := ticketRouters(t, 2, true)
	exp := time.Now().Add(30 * time.Second)
	ctx := context.Background()
	c.Kill()
	start := time.Now()
	if !rts[0].consumeTicketEverywhere(ctx, "jti-1", exp) {
		t.Fatal("the first use of a ticket was refused with Redis down")
	}
	if rts[0].consumeTicketEverywhere(ctx, "jti-1", exp) {
		t.Error("with Redis down the same replica accepted a ticket twice")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("two uses took %v with Redis down", d)
	}
}

// The key outlives the ticket by a margin and no longer than the declared TTL.
func TestWSTicket_TheKeyExpires(t *testing.T) {
	c, rts := ticketRouters(t, 1, true)
	rts[0].consumeTicketEverywhere(context.Background(), "jti-1", time.Now().Add(30*time.Second))
	keys := c.Redis.Keys()
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want the ticket's", keys)
	}
	if ttl := c.Redis.TTL(keys[0]); ttl < 30*time.Second || ttl > wsTicketKey.Spec().TTL {
		t.Errorf("ttl = %v, want the ticket's remaining life plus a margin, at most %v", ttl, wsTicketKey.Spec().TTL)
	}
}
