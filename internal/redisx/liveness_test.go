package redisx

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A replica's liveness key: written at start with its TTL, kept while it runs, gone at once on a clean stop, and gone
// by itself a TTL after the last write of a replica that died.
func TestLiveness_KeyLifecycle(t *testing.T) {
	prev := LivenessEvery
	LivenessEvery = 20 * time.Millisecond
	t.Cleanup(func() { LivenessEvery = prev })
	mr, client, _ := newHealth(t)
	ctx := context.Background()
	l := NewLiveness(client, "pod-1#abc")
	l.Start()
	l.Start() // a second start does nothing
	if alive, err := ReplicaAlive(ctx, client, "pod-1#abc"); err != nil || !alive {
		t.Fatalf("ReplicaAlive after start = %v, %v", alive, err)
	}
	if ttl := mr.TTL(ReplicaLiveKey.Key("pod-1#abc")); ttl <= 0 || ttl > LivenessTTL {
		t.Errorf("TTL = %v, want 0 < ttl <= %v", ttl, LivenessTTL)
	}
	// The writes go on: past one TTL of server time the key is still there.
	mr.FastForward(LivenessTTL - time.Second)
	time.Sleep(100 * time.Millisecond)
	mr.FastForward(2 * time.Second)
	if alive, _ := ReplicaAlive(ctx, client, "pod-1#abc"); !alive {
		t.Error("a running replica's key lapsed")
	}
	l.Stop()
	l.Stop()
	if alive, _ := ReplicaAlive(ctx, client, "pod-1#abc"); alive {
		t.Error("the key outlived a clean stop")
	}

	// A replica that died: its last write lapses on its own.
	LivenessEvery = time.Hour
	dead := NewLiveness(client, "pod-2#def")
	dead.Start()
	defer dead.Stop()
	mr.FastForward(LivenessTTL + time.Second)
	if alive, err := ReplicaAlive(ctx, client, "pod-2#def"); err != nil || alive {
		t.Errorf("a dead replica's key = %v, %v; want gone", alive, err)
	}
}

// Redis down: ReplicaAlive says it does not know, never "gone". A nil writer and one without a name do nothing.
func TestLiveness_UnreadableIsAnError(t *testing.T) {
	mr, client, _ := newHealth(t)
	mr.Close()
	if _, err := ReplicaAlive(context.Background(), client, "pod-1#abc"); err == nil {
		t.Error("ReplicaAlive with Redis down answered without an error")
	}
	if NewLiveness(nil, "x") != nil || NewLiveness(client, "") != nil {
		t.Error("a writer without a client or a name is not nil")
	}
	var l *Liveness
	l.Start()
	l.Stop()
}

// The writer keeps "key present" meaning "commands are received": nothing is written while the replica is not
// listening, a kick writes at once, and OnStale runs once no write has succeeded for two refreshes (well before a key
// written at the last success can lapse), and stops running when writes succeed again.
func TestLiveness_ListeningKickAndStale(t *testing.T) {
	prev := LivenessEvery
	LivenessEvery = 20 * time.Millisecond
	t.Cleanup(func() { LivenessEvery = prev })
	mr, client, _ := newHealth(t)
	var listening atomic.Bool
	var stale atomic.Int32
	l := NewLiveness(client, "pod-3#ghi")
	l.WhileListening(listening.Load)
	l.OnStale(func() { stale.Add(1) })
	l.Start()
	defer l.Stop()
	key := ReplicaLiveKey.Key("pod-3#ghi")
	if mr.Exists(key) {
		t.Fatal("a replica that does not listen yet wrote its key")
	}
	eventually(t, "OnStale while not listening", func() bool { return stale.Load() > 0 })
	listening.Store(true)
	l.Kick()
	eventually(t, "the key written once listening", func() bool { return mr.Exists(key) })
	time.Sleep(5 * LivenessEvery) // a success resets the clock: no OnStale while the writes succeed
	n := stale.Load()
	time.Sleep(5 * LivenessEvery)
	if stale.Load() != n {
		t.Error("OnStale ran while the writes succeed")
	}
	if l.staleAfter <= 0 || l.staleAfter > LivenessTTL-LivenessEvery {
		t.Errorf("staleAfter = %v, want within (0, %v]", l.staleAfter, LivenessTTL-LivenessEvery)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
