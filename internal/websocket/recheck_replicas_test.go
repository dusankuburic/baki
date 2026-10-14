package websocket

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/invalidation"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/testutil/redisreplica"
)

// An access change made on replica A flags the sockets of replica B at once, instead of at B's 2-minute pass: the
// real subscribers, the real fan-out and the real appliers, on two hubs.
func TestRecheck_AnAccessChangeOnOneReplicaFlagsThePeersSockets(t *testing.T) {
	c := redisreplica.New(t, 2)
	hubs := make([]*Hub, 2)
	buses := make([]*eventbus.Bus, 2)
	probes := make([]*Client, 2)
	for i, r := range c.Replicas {
		hubs[i] = NewHub()
		t.Cleanup(hubs[i].Close)
		probes[i] = joinWith(t, hubs[i], "probe", "nobody", FlowOwner{UserID: "nobody"}, "j-probe")
		buses[i] = eventbus.New(nil, time.Second)
		SubscribeAccessRechecks(buses[i], hubs[i])
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
	// The resync every subscription starts with flags every socket; wait for it on each hub before the test joins its own.
	for i := range probes {
		redisreplica.Eventually(t, 5*time.Second, "the first resync on each hub", func() bool { return flagged(probes[i]) })
	}

	onA := joinWith(t, hubs[0], "f1", "bob", FlowOwner{UserID: "alice", OrgID: "o1"}, "j-bob")
	onB := joinWith(t, hubs[1], "f1", "bob", FlowOwner{UserID: "alice", OrgID: "o1"}, "j-bob")
	other := joinWith(t, hubs[1], "f2", "carol", FlowOwner{UserID: "carol"}, "j-carol")
	flagged(onA)
	flagged(onB)
	flagged(other)

	// A logout on A: the session's credential. A flags its own socket through its own subscriber; B's is flagged by
	// the nudge, and carol's, on another credential, is not.
	buses[0].Publish(context.Background(), events.AccessRevoked{UserID: "bob", JTI: "j-bob"})
	redisreplica.Eventually(t, 5*time.Second, "the peer's socket to be flagged", func() bool { return flagged(onB) })
	if !flagged(onA) {
		t.Error("the origin's own socket was not flagged by its own subscriber")
	}
	if flagged(other) {
		t.Error("a socket on another credential was flagged")
	}

	// A change that rolls back reaches nobody.
	ctx, reg := txhook.With(context.Background())
	buses[0].Publish(ctx, events.OrgMemberChanged{OrgID: "o1", UserID: "bob", Kind: events.OrgMemberRemoved})
	reg.Abort()
	time.Sleep(200 * time.Millisecond)
	if flagged(onB) {
		t.Error("the peer's socket was flagged for a change that rolled back")
	}

	// A removed member: every socket of that user on the peer.
	buses[0].Publish(context.Background(), events.OrgMemberChanged{OrgID: "o1", UserID: "bob", Kind: events.OrgMemberRemoved})
	redisreplica.Eventually(t, 5*time.Second, "the peer's socket of the removed member", func() bool { return flagged(onB) })
}
