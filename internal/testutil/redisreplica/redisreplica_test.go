package redisreplica_test

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/testutil/redisreplica"
)

// The replicas are separate in everything but the server: distinct IDs, distinct clients and pools, one shared
// keyspace and one shared pub/sub.
func TestCluster_ReplicasShareOneRedis(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 3)
	if len(c.Replicas) != 3 {
		t.Fatalf("replicas = %d, want 3", len(c.Replicas))
	}
	ids := map[string]bool{}
	clients := map[any]bool{}
	for _, r := range c.Replicas {
		ids[r.ID] = true
		clients[r.Client] = true
		if r.Health == nil || !r.Health.Configured() {
			t.Fatalf("replica %s has no configured Health", r.ID)
		}
	}
	if len(ids) != 3 || len(clients) != 3 {
		t.Fatalf("ids %v, %d distinct clients; want 3 of each", ids, len(clients))
	}

	a, b := c.Replicas[0], c.Replicas[1]
	if err := a.Client.Set(ctx, "k", "v", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Client.Get(ctx, "k").Result(); err != nil || got != "v" {
		t.Fatalf("replica 2 read %q, %v; want the value replica 1 wrote", got, err)
	}

	sub := b.Client.Subscribe(ctx, "baki:test:channel")
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Client.Publish(ctx, "baki:test:channel", "hello").Err(); err != nil {
		t.Fatal(err)
	}
	msg, err := sub.ReceiveMessage(ctx)
	if err != nil || msg.Payload != "hello" {
		t.Fatalf("subscriber on replica 2 got %q, %v", msg.Payload, err)
	}
}

// Kill cuts every replica off and degrades each one's Health; Restore brings the data and the health back.
func TestCluster_KillAndRestore(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 2)
	a, b := c.Replicas[0], c.Replicas[1]
	if err := a.Client.Set(ctx, "k", "v", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := a.Health.Ping(ctx); err != nil || a.Health.Degraded() {
		t.Fatalf("healthy ping: err %v, degraded %v", err, a.Health.Degraded())
	}

	c.Kill()
	for _, r := range c.Replicas {
		if err := r.Client.Get(ctx, "k").Err(); err == nil {
			t.Fatalf("replica %s: a command succeeded against a killed Redis", r.ID)
		}
		if err := r.Health.Ping(ctx); err == nil || !r.Health.Degraded() {
			t.Fatalf("replica %s: ping err %v, degraded %v; want an error and degraded", r.ID, err, r.Health.Degraded())
		}
	}

	c.Restore(t)
	redisreplica.Eventually(t, 2*time.Second, "both replicas healthy again", func() bool {
		return a.Health.Ping(ctx) == nil && b.Health.Ping(ctx) == nil
	})
	if a.Health.Degraded() || b.Health.Degraded() {
		t.Fatal("still degraded after a successful ping")
	}
	if got, err := b.Client.Get(ctx, "k").Result(); err != nil || got != "v" {
		t.Fatalf("after Restore replica 2 read %q, %v; want the data written before the outage", got, err)
	}
}

func TestPrevFixture_ReadsTheCapturedRelease(t *testing.T) {
	b := redisreplica.PrevFixture(t, "0043", "revocation_message.json")
	if len(b) == 0 || b[0] != '{' {
		t.Fatalf("fixture = %q, want a JSON object", b)
	}
}
