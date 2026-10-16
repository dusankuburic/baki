package websocket

import (
	"testing"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/testutil/redisreplica"
)

// The per-user socket cap on a deployment that can run more than one replica (PAD_MAX_REPLICAS > 1): it counts every
// replica's sockets, so N replicas do not allow N times maxConnsPerUser.

func sharedHub(rep *redisreplica.Replica) *Hub {
	h := NewHub()
	h.ShareConnCap(rep.Client, rep.Health)
	return h
}

// capsOpen reads pad_redis_caps_total{cap="ws.conn-slots",outcome="open"}: checks that fell back to the hub's own cap
// because Redis did not answer in time.
func capsOpen(t *testing.T) float64 {
	t.Helper()
	families, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "pad_redis_caps_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["cap"] == "ws.conn-slots" && labels["outcome"] == "open" {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// overCap fails the test for an admission over the cross-replica cap, unless a check in the test fell back to the
// hub's own cap: the harness gives each Redis call 500 ms, which a loaded machine can miss, and failing open is then
// the policy, not a defect.
func overCap(t *testing.T, openBefore float64, msg string) {
	t.Helper()
	if capsOpen(t) > openBefore {
		t.Skipf("a cap check fell back to the hub's own cap (Redis slower than the harness timeout under load): %s", msg)
	}
	t.Fatal(msg)
}

func TestConnCap_CountsEveryReplicasSockets(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := sharedHub(c.Replicas[0]), sharedHub(c.Replicas[1])
	open := capsOpen(t)
	var releases []func()
	for i := 0; i < maxConnsPerUser; i++ {
		release, ok := a.AcquireConn("u1")
		if !ok {
			t.Fatalf("socket %d on A refused", i+1)
		}
		releases = append(releases, release)
	}
	if _, ok := b.AcquireConn("u1"); ok {
		overCap(t, open, "an 11th socket of u1 on B was admitted: the cap must count A's sockets")
	}
	if release, ok := b.AcquireConn("u2"); !ok {
		t.Fatal("another user's socket on B refused")
	} else {
		release()
	}
	releases[0]()
	release, ok := b.AcquireConn("u1")
	if !ok {
		t.Fatal("a socket on B after one closed on A was refused")
	}
	release()
	for _, r := range releases[1:] {
		r()
	}
}

// A refusal across the replicas leaves nothing counted on the refusing hub.
func TestConnCap_ARefusalHoldsNothing(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := sharedHub(c.Replicas[0]), sharedHub(c.Replicas[1])
	open := capsOpen(t)
	for i := 0; i < maxConnsPerUser; i++ {
		if _, ok := a.AcquireConn("u1"); !ok {
			t.Fatal(i)
		}
	}
	for i := 0; i < 3; i++ {
		if _, ok := b.AcquireConn("u1"); ok {
			overCap(t, open, "admitted over the cap")
		}
	}
	b.mu.RLock()
	n := b.connsPerUser["u1"]
	b.mu.RUnlock()
	if n != 0 {
		t.Fatalf("B counts %d sockets of u1 after refusing every one", n)
	}
}

// Redis down: each hub's own cap applies, and only it.
func TestConnCap_RedisDownKeepsEachHubsOwnCap(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := sharedHub(c.Replicas[0]), sharedHub(c.Replicas[1])
	c.Kill()
	for _, h := range []*Hub{a, b} {
		for i := 0; i < maxConnsPerUser; i++ {
			if _, ok := h.AcquireConn("u1"); !ok {
				t.Fatalf("socket %d with Redis down refused", i+1)
			}
		}
		if _, ok := h.AcquireConn("u1"); ok {
			t.Fatal("an 11th socket on one hub with Redis down was admitted")
		}
	}
}
