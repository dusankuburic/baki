package api

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/invalidation"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/testutil/redisreplica"
)

// A logout (or a deleted API token) on replica A ends the token's event streams on replica B at once, not at B's
// 2-minute blacklist check: the real subscriber, the real fan-out and the applier, on two event managers.
func TestEvents_ARevocationOnOneReplicaEndsThePeersStreams(t *testing.T) {
	c := redisreplica.New(t, 2)
	managers := make([]*EventManager, 2)
	buses := make([]*eventbus.Bus, 2)
	for i, r := range c.Replicas {
		managers[i] = NewEventManager(make(chan struct{}))
		buses[i] = eventbus.New(nil, time.Second)
		managers[i].SubscribeRevocations(buses[i])
		managers[i].SubscribeRemoteRevocations(buses[i])
		stop := invalidation.Install(r.Client, r.Health, buses[i], r.ID, invalidation.WithHeartbeat(time.Hour))
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			stop(ctx)
		})
	}
	redisreplica.Eventually(t, 5*time.Second, "both fan-outs subscribed", func() bool {
		return c.Redis.PubSubNumSub("baki:inval")["baki:inval"] == 2
	})

	onB, stopB := managers[1].watchRevocation("session-1")
	defer stopB()
	otherOnB, stopOtherB := managers[1].watchRevocation("session-2")
	defer stopOtherB()
	onA, stopA := managers[0].watchRevocation("session-1")
	defer stopA()

	buses[0].Publish(context.Background(), events.AccessRevoked{UserID: "u1", JTI: "session-1"})

	select {
	case <-onB:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer's stream of the revoked token is still open")
	}
	select {
	case <-onA:
	case <-time.After(5 * time.Second):
		t.Fatal("the origin's own stream of the revoked token is still open")
	}
	select {
	case <-otherOnB:
		t.Error("another token's stream on the peer ended")
	case <-time.After(100 * time.Millisecond):
	}
}
