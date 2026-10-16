package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/redisreplica"
	"pad-core/models"
)

// The stream start and the stream commands on a deployment that can run more than one replica (PAD_MAX_REPLICAS > 1;
// ADR 0033, addendum (close-out)): the stream cap and the clientStreamId check hold across the replicas, and a stop
// for a stream whose replica died is answered as an ended stream once the replica's liveness key has lapsed.

// newSharedReplica is a ChatService of a multi-replica deployment on one harness replica, built as internal/di builds
// it (NewChatService with MultiReplica), and listening for commands.
func newSharedReplica(t *testing.T, r *redisreplica.Replica) *ChatService {
	t.Helper()
	ai.RegisterProvider("cmd-blocking", func(string) ai.Provider { return &cmdProvider{FakeProvider: &testutil.FakeProvider{}} })
	svc := NewChatService(&testutil.CountingNotifier{}, t.TempDir(), &FlowService{}, &AnalysisService{}, nilSettings{}, keyedFactory(), nil, "",
		ChatOptions{Resume: r.Client, Instance: r.ID, MultiReplica: true, Health: r.Health})
	svc.StartCommands()
	t.Cleanup(svc.StopCommands)
	redisreplica.Eventually(t, 3*time.Second, r.ID+" command bus subscribed", svc.cluster.bus.Ready)
	return svc
}

// startStreamOn starts a blocking stream for scope on svc under id and returns the error StreamChatMessage gave.
func startStreamOn(t *testing.T, svc *ChatService, scope, id string) error {
	t.Helper()
	_, err := svc.StreamChatMessage(context.Background(), scope, &models.FlowDocument{ID: "f1"}, nil, models.ChatRequest{
		Provider: "cmd-blocking", Model: "fake-model", UserMessage: "hi", ClientStreamID: id,
	})
	if err == nil {
		// Every stream of svc is ended before the wait: a wait after one stop would wait for the others.
		t.Cleanup(func() { svc.cancelAll(); svc.workers.Wait() })
	}
	return err
}

// The cap of 3 concurrent streams per caller counts the streams of every replica: 3 on A leave none on B, another
// caller is not affected, and a stream that ends on A frees a slot for B.
func TestStreamCap_CountsEveryReplicasStreams(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newSharedReplica(t, c.Replicas[0]), newSharedReplica(t, c.Replicas[1])
	var onA []string
	for i := 0; i < maxConcurrentStreamsPerScope; i++ {
		id := uuid.NewString()
		if err := startStreamOn(t, a, "u1", id); err != nil {
			t.Fatalf("stream %d on A: %v", i+1, err)
		}
		onA = append(onA, id)
	}
	if err := startStreamOn(t, b, "u1", uuid.NewString()); !errors.Is(err, ErrTooManyChatStreams) {
		t.Fatalf("a 4th stream of u1 on B = %v, want ErrTooManyChatStreams: the cap must count A's streams", err)
	}
	if err := startStreamOn(t, b, "u2", uuid.NewString()); err != nil {
		t.Fatalf("another caller's stream on B: %v", err)
	}
	a.CancelStream(onA[0])
	waitStreamFinished(t, a, onA[0])
	if err := startStreamOn(t, b, "u1", uuid.NewString()); err != nil {
		t.Fatalf("a stream of u1 on B after one ended on A: %v", err)
	}
}

// A replica never exceeds its own cap, whatever Redis says, and Redis down leaves exactly that cap (fail open).
func TestStreamCap_RedisDownKeepsEachReplicasOwnCap(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newSharedReplica(t, c.Replicas[0]), newSharedReplica(t, c.Replicas[1])
	c.Kill()
	for _, svc := range []*ChatService{a, b} {
		for i := 0; i < maxConcurrentStreamsPerScope; i++ {
			if err := startStreamOn(t, svc, "u1", uuid.NewString()); err != nil {
				t.Fatalf("stream %d with Redis down: %v (the cap fails open to the replica's own)", i+1, err)
			}
		}
		if err := startStreamOn(t, svc, "u1", uuid.NewString()); !errors.Is(err, ErrTooManyChatStreams) {
			t.Fatalf("a 4th stream on one replica with Redis down = %v, want ErrTooManyChatStreams", err)
		}
	}
}

// The slots of a replica that died stop counting once their lease has passed, with nobody deleting them.
func TestStreamCap_ADeadReplicasSlotsLapse(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newSharedReplica(t, c.Replicas[0]), newSharedReplica(t, c.Replicas[1])
	for i := 0; i < maxConcurrentStreamsPerScope; i++ {
		if err := startStreamOn(t, a, "u1", uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	if err := startStreamOn(t, b, "u1", uuid.NewString()); !errors.Is(err, ErrTooManyChatStreams) {
		t.Fatalf("premise: B refuses while A's slots count, got %v", err)
	}
	// A "dies": nothing refreshes its slots. B asks a lease later, on the server's clock, which judges the leases.
	c.Redis.SetTime(time.Now().Add(redisx.SlotLease + time.Second))
	if err := startStreamOn(t, b, "u1", uuid.NewString()); err != nil {
		t.Fatalf("a stream on B once A's slots lapsed: %v", err)
	}
}

// A clientStreamId is one stream across the replicas: the id of a stream running on A is refused on B, as on A, and
// stays refused for as long as A keeps the finished stream (resumeRetention), then is free.
func TestStreamID_IsClaimedAcrossReplicas(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newSharedReplica(t, c.Replicas[0]), newSharedReplica(t, c.Replicas[1])
	id := uuid.NewString()
	if err := startStreamOn(t, a, "u1", id); err != nil {
		t.Fatal(err)
	}
	if err := startStreamOn(t, b, "u2", id); !errors.Is(err, errStreamIDInUse) {
		t.Fatalf("the same clientStreamId on B = %v, want errStreamIDInUse", err)
	}
	a.CancelStream(id)
	waitStreamFinished(t, a, id)
	redisreplica.Eventually(t, 3*time.Second, "the claim shortened to the retention", func() bool {
		ttl := c.Redis.TTL(chatStreamIDKey.Key(id))
		return ttl > 0 && ttl <= resumeRetention
	})
	if err := startStreamOn(t, b, "u1", id); !errors.Is(err, errStreamIDInUse) {
		t.Fatalf("the id of a stream A finished a moment ago, on B = %v, want errStreamIDInUse", err)
	}
	c.Redis.FastForward(resumeRetention + time.Second)
	if err := startStreamOn(t, b, "u1", id); err != nil {
		t.Fatalf("the id after the retention: %v", err)
	}
}

// A start that is refused (the cap) gives its id back: the client may send the same id again.
func TestStreamID_ARefusedStartReleasesTheClaim(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newSharedReplica(t, c.Replicas[0]), newSharedReplica(t, c.Replicas[1])
	var running []string
	for i := 0; i < maxConcurrentStreamsPerScope; i++ {
		id := uuid.NewString()
		if err := startStreamOn(t, a, "u1", id); err != nil {
			t.Fatal(err)
		}
		running = append(running, id)
	}
	id := uuid.NewString()
	if err := startStreamOn(t, b, "u1", id); !errors.Is(err, ErrTooManyChatStreams) {
		t.Fatalf("premise: the 4th is refused, got %v", err)
	}
	if c.Redis.Exists(chatStreamIDKey.Key(id)) {
		t.Fatal("a refused start left its id claimed")
	}
	a.CancelStream(running[0])
	waitStreamFinished(t, a, running[0])
	if err := startStreamOn(t, b, "u1", id); err != nil {
		t.Fatalf("the refused id sent again once a slot is free: %v", err)
	}
}

// With one replica configured nothing of this touches Redis: the cap and the id check are the replica's own (a Redis
// maintenance window costs a single replica nothing), and two such services do not see each other.
func TestStreamCap_OneReplicaStaysLocal(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	for _, svc := range []*ChatService{a, b} {
		for i := 0; i < maxConcurrentStreamsPerScope; i++ {
			if err := startStreamOn(t, svc, "u1", uuid.NewString()); err != nil {
				t.Fatalf("stream %d: %v", i+1, err)
			}
		}
	}
	for _, k := range c.Redis.Keys() {
		if s, ok := redisx.Match(k); ok && (s.Name == chatStreamSlotsKey.Name() || s.Name == chatStreamIDKey.Name() || s.Name == redisx.ReplicaLiveKey.Name()) {
			t.Errorf("a single-replica deployment wrote %s", k)
		}
	}
}

// killReplica makes svc look like a process that died: nobody listens on its command channel and nothing refreshes its
// liveness key (redisx.LivenessEvery is long in these tests), which is left in place to lapse.
func killReplica(svc *ChatService) { svc.cluster.bus.Stop() }

// steadyView moves svc's clock past a liveness TTL, as if its own view of Redis had been steady that long: the
// condition for believing a missing key (chatCluster.steadyView). The harness's replicas have just started.
func steadyView(svc *ChatService) {
	svc.cluster.now = func() time.Time { return time.Now().Add(redisx.LivenessTTL + time.Second) }
}

// slowLiveness keeps the liveness writer from refreshing during a test, so a test decides when the key lapses.
func slowLiveness(t *testing.T) {
	prev := redisx.LivenessEvery
	redisx.LivenessEvery = time.Hour
	t.Cleanup(func() { redisx.LivenessEvery = prev })
}

// A stop for a stream whose replica died: 503 while the replica's liveness key says it may be alive, then, once it has
// lapsed, answered as a stop for an ended stream (ok) and the stale snapshot is gone, so a resume reports the stream
// interrupted instead of running.
func TestCommands_StopForADeadReplicasStreamIsAnEndedStream(t *testing.T) {
	slowLiveness(t)
	c := redisreplica.New(t, 2)
	a, b := newSharedReplica(t, c.Replicas[0]), newSharedReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	snap, _ := a.resumeBackplane().Load(context.Background(), id)
	if !snap.Live {
		t.Fatalf("premise: the snapshot of a multi-replica owner says it keeps a liveness key: %+v", snap)
	}
	killReplica(a)

	if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("a stop while the owner's liveness key is still there = %v, want ErrReplicaUnreachable", err)
	}
	c.Redis.FastForward(redisx.LivenessTTL + time.Second)
	steadyView(b)
	gone := promtest.ToFloat64(chatCommandsTotal.WithLabelValues(opCancel, "origin", "owner_gone"))
	if err := b.CancelStreamAs(context.Background(), "u1", id); err != nil {
		t.Fatalf("a stop once the owner's liveness key lapsed = %v, want nil (the stream ended with its replica)", err)
	}
	if got := promtest.ToFloat64(chatCommandsTotal.WithLabelValues(opCancel, "origin", "owner_gone")) - gone; got != 1 {
		t.Errorf("owner_gone count += %v, want 1", got)
	}
	if c.Redis.Exists(resumeKey.Key(id)) {
		t.Error("the dead stream's snapshot is still there")
	}
	if _, err := b.ResumeStream(context.Background(), id, 0); err == nil {
		t.Error("a resume of the dead stream found it: the client would wait for a stream that never ends")
	}
}

// The owner alive (its key is there) but silent, and Redis down: still 503, never an ok.
func TestCommands_StopForASilentLiveOwnerOrWithRedisDownIs503(t *testing.T) {
	prev := chatCommandWait
	chatCommandWait = 150 * time.Millisecond
	t.Cleanup(func() { chatCommandWait = prev })
	c := redisreplica.New(t, 2)
	a, b := newSharedReplica(t, c.Replicas[0]), newSharedReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	a.cluster.bus.Stop()
	a.cluster.bus.Start(func(context.Context, []byte) []byte { return nil })
	redisreplica.Eventually(t, 3*time.Second, "A subscribed again", a.cluster.bus.Ready)
	if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("a stop for a live owner that does not answer = %v, want ErrReplicaUnreachable", err)
	}
	c.Kill()
	if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("a stop with Redis down = %v, want ErrReplicaUnreachable", err)
	}
	if !streamRunning(a, id) {
		t.Fatal("the stream was stopped")
	}
}

// An approval whose replica died is no longer pending: a decision is answered as for a decided or expired approval
// (ErrNoPendingFix, 404) once the replica's liveness key lapsed, its record is removed and its claim given back.
func TestCommands_DecisionForADeadReplicasApprovalIsNoPending(t *testing.T) {
	slowLiveness(t)
	c := redisreplica.New(t, 2)
	a, b := newSharedReplica(t, c.Replicas[0]), newSharedReplica(t, c.Replicas[1])
	a.activeStreams.Store("s9", &streamCtl{ownerID: "u1", streamID: "s9"})
	go func() {
		_, _ = a.requestApproval(context.Background(), "s9", approvalRequest{id: "p9", tool: "apply_fix"}, func(string, map[string]interface{}) {}, nil)
	}()
	redisreplica.Eventually(t, 3*time.Second, "the pending approval in Redis", func() bool { return c.Redis.Exists(chatApprovalKey.Key("p9")) })
	killReplica(a)
	if err := b.DecideApproval(context.Background(), "u1", "s9", "p9", true, nil); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("a decision while the owner's key is there = %v, want ErrReplicaUnreachable", err)
	}
	c.Redis.FastForward(redisx.LivenessTTL + time.Second)
	steadyView(b)
	if err := b.DecideApproval(context.Background(), "u1", "s9", "p9", true, nil); !errors.Is(err, ErrNoPendingFix) {
		t.Fatalf("a decision once the owner's key lapsed = %v, want ErrNoPendingFix", err)
	}
	if c.Redis.Exists(chatApprovalKey.Key("p9")) {
		t.Error("the dead approval's record is still there")
	}
	if c.Redis.Exists(chatDecisionKey.Key("p9")) {
		t.Error("the claim of a decision nobody can receive was kept")
	}
}

// A replica that stops cleanly removes its liveness key at once: a stop for a stream it left behind is not 503 for 15 s.
func TestLiveness_ACleanStopRemovesTheKey(t *testing.T) {
	c := redisreplica.New(t, 2)
	a := newSharedReplica(t, c.Replicas[0])
	key := redisx.ReplicaLiveKey.Key(a.cluster.instance)
	// Written once its command subscription is up (Liveness.WhileListening), a moment after it starts.
	redisreplica.Eventually(t, 3*time.Second, "a running multi-replica replica's liveness key", func() bool { return c.Redis.Exists(key) })
	if ttl := c.Redis.TTL(key); ttl <= 0 || ttl > redisx.LivenessTTL {
		t.Errorf("liveness TTL = %v, want within %v", ttl, redisx.LivenessTTL)
	}
	a.StopCommands()
	if c.Redis.Exists(key) {
		t.Error("the liveness key outlived a clean stop")
	}
}
