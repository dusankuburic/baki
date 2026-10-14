package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/redisreplica"
)

// The fix round of REVIEW_REDIS M1, M2, M3, L1, L7, L10 and REVIEW_COMPAT M6 for the stream commands (ADR 0033).

// pendingApproval puts an approval for stream s1 pending on svc and returns the channel its decision arrives on.
func pendingApproval(t *testing.T, c *redisreplica.Cluster, svc *ChatService, proposal string) <-chan approvalDecision {
	t.Helper()
	svc.activeStreams.Store("s1", &streamCtl{ownerID: "u1", streamID: "s1"})
	done := make(chan approvalDecision, 1)
	go func() {
		dec, _ := svc.requestApproval(context.Background(), "s1", approvalRequest{id: proposal, tool: "apply_fix"}, func(string, map[string]interface{}) {}, nil)
		done <- dec
	}()
	redisreplica.Eventually(t, 3*time.Second, "the pending approval in Redis", func() bool { return c.Redis.Exists(chatApprovalKey.Key(proposal)) })
	return done
}

// slowOwner makes svc answer commands after d, as a busy replica does.
func slowOwner(t *testing.T, svc *ChatService, d time.Duration, done *atomic.Int64) {
	t.Helper()
	svc.StopCommands()
	svc.cluster.bus.Start(func(ctx context.Context, payload []byte) []byte {
		time.Sleep(d)
		out := svc.handleChatCommand(ctx, payload)
		done.Store(time.Now().UnixNano())
		return out
	})
	redisreplica.Eventually(t, 3*time.Second, "the owner subscribed again", svc.cluster.bus.Ready)
}

type timedResult struct {
	err error
	at  time.Time
}

func decideAsync(svc *ChatService, proposal string, approved bool) <-chan timedResult {
	out := make(chan timedResult, 1)
	go func() {
		err := svc.DecideApproval(context.Background(), "u1", "s1", proposal, approved, nil)
		out <- timedResult{err, time.Now()}
	}()
	return out
}

// The replica that lost the claim does not answer until the winner's decision has an outcome: an ok is only true
// once the decision reached the owner (M1, scenario A: a double click landing on two replicas).
func TestCommands_TheClaimLoserWaitsForTheWinnersOutcome(t *testing.T) {
	c := redisreplica.New(t, 3)
	a, b, d := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1]), newCmdReplica(t, c.Replicas[2])
	var ownerDone atomic.Int64
	done := pendingApproval(t, c, a, "p1")
	slowOwner(t, a, 300*time.Millisecond, &ownerDone)

	start := time.Now()
	rb, rd := decideAsync(b, "p1", true), decideAsync(d, "p1", true)
	for name, ch := range map[string]<-chan timedResult{"b": rb, "d": rd} {
		select {
		case r := <-ch:
			if r.err != nil {
				t.Errorf("%s: %v, want ok", name, r.err)
			}
			if owner := ownerDone.Load(); owner == 0 || r.at.UnixNano() < owner {
				t.Errorf("%s answered ok %v after the start, before the owner had decided: the loser's ok proves nothing", name, r.at.Sub(start))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not answer", name)
		}
	}
	if dec := <-done; !dec.approved {
		t.Errorf("owner received %+v, want the approval", dec)
	}
}

// The winner fails to deliver: the loser must not report success for a decision nobody received. It takes the claim
// over, sends itself, and, with the owner gone, reports the owner unreachable (M1).
func TestCommands_TheClaimLoserDoesNotAnswerOkWhenTheWinnerFails(t *testing.T) {
	c := redisreplica.New(t, 3)
	a, b, d := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1]), newCmdReplica(t, c.Replicas[2])
	done := pendingApproval(t, c, a, "p1")

	won, claim, err := b.cluster.claimDecision(context.Background(), "p1")
	if err != nil || !won {
		t.Fatalf("setup: claim = (%v, %v)", won, err)
	}
	res := decideAsync(d, "p1", true)
	select {
	case r := <-res:
		t.Fatalf("the loser answered %v while the winner's decision was in flight: a false ok", r.err)
	case <-time.After(100 * time.Millisecond):
	}

	a.StopCommands() // the owner is gone; the winner's delivery failed and it gave the claim back
	redisreplica.Eventually(t, 3*time.Second, "the owner's subscription gone", func() bool {
		return c.Redis.PubSubNumSub(chatCmdChannel.Key("replica-1"))[chatCmdChannel.Key("replica-1")] == 0
	})
	b.cluster.releaseDecision("p1", claim)

	select {
	case r := <-res:
		if !errors.Is(r.err, ErrReplicaUnreachable) {
			t.Fatalf("loser after the winner failed = %v, want ErrReplicaUnreachable (it sent the decision itself and nobody listened)", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loser never answered")
	}
	select {
	case dec := <-done:
		t.Fatalf("the owner received %+v from a failed delivery", dec)
	default:
	}
}

// A winner that died after claiming leaves its claim in Redis for a minute; a decision arriving meanwhile must not be
// swallowed by it (M1: a crash between the claim and the call).
func TestCommands_AClaimWhoseOwnerDiedIsTakenOver(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	done := pendingApproval(t, c, a, "p1")
	old, _ := json.Marshal(map[string]any{"v": 1, "kind": "chat.decision", "by": "ghost#00000000", "token": "t", "state": "sending",
		"at": time.Now().Add(-30 * time.Second).UnixMilli()})
	c.Redis.Set(chatDecisionKey.Key("p1"), string(old))
	c.Redis.SetTTL(chatDecisionKey.Key("p1"), decisionKeyTTL)

	if err := b.DecideApproval(context.Background(), "u1", "s1", "p1", true, nil); err != nil {
		t.Fatalf("decision behind a dead winner's claim: %v", err)
	}
	select {
	case dec := <-done:
		if !dec.approved {
			t.Errorf("owner received %+v, want the approval", dec)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the decision never reached the owner: it was swallowed by a claim nobody is working on")
	}
}

// L10: the owner decided but its reply was lost. The first attempt is told to retry; the retry must not be told
// "no such approval" for a decision that WAS applied.
func TestCommands_ARetryAfterALostReplyIsNotANotFound(t *testing.T) {
	prev := chatCommandWait
	chatCommandWait = 150 * time.Millisecond
	t.Cleanup(func() { chatCommandWait = prev })
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	done := pendingApproval(t, c, a, "p1")
	// The owner decides and its answer never leaves.
	a.StopCommands()
	a.cluster.bus.Start(func(ctx context.Context, payload []byte) []byte { _ = a.handleChatCommand(ctx, payload); return nil })
	redisreplica.Eventually(t, 3*time.Second, "the owner subscribed again", a.cluster.bus.Ready)

	if err := b.DecideApproval(context.Background(), "u1", "s1", "p1", true, nil); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("first attempt with the reply lost = %v, want ErrReplicaUnreachable", err)
	}
	if dec := <-done; !dec.approved {
		t.Fatalf("owner received %+v, want the approval", dec)
	}
	redisreplica.Eventually(t, 3*time.Second, "the approval dropped", func() bool { return !c.Redis.Exists(chatApprovalKey.Key("p1")) })

	if err := b.DecideApproval(context.Background(), "u1", "s1", "p1", true, nil); err != nil {
		t.Errorf("retry of the decision that was applied = %v, want ok (it is recorded)", err)
	}
	// A different decision is too late, as it would be after any first decision.
	if err := b.DecideApproval(context.Background(), "u1", "s1", "p1", false, nil); !errors.Is(err, ErrNoPendingFix) {
		t.Errorf("the opposite decision after the first = %v, want ErrNoPendingFix", err)
	}
}

// A reply status from a peer is not a metric label until it is one of the known ones (L1): a newer or hostile peer
// cannot mint series.
func TestCommands_AnUnknownReplyStatusIsOneLabel(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	a.StopCommands()
	a.cluster.bus.Start(func(_ context.Context, payload []byte) []byte {
		var cmd chatCommand
		_ = json.Unmarshal(payload, &cmd)
		out, _ := json.Marshal(chatReply{V: 1, Kind: chatReplyKind, RequestID: cmd.RequestID, Status: "status-" + uuid.NewString()})
		return out
	})
	redisreplica.Eventually(t, 3*time.Second, "the owner subscribed again", a.cluster.bus.Ready)
	before := promtest.ToFloat64(chatCommandsTotal.WithLabelValues("cancel", "origin", "other"))
	for range 3 {
		if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
			t.Fatalf("CancelStreamAs with an unknown status = %v, want ErrReplicaUnreachable", err)
		}
	}
	if got := promtest.ToFloat64(chatCommandsTotal.WithLabelValues("cancel", "origin", "other")) - before; got != 3 {
		t.Errorf("other count += %v, want 3", got)
	}
	if labelValueSeen(t, "pad_chat_commands_total", "status-") {
		t.Error("a peer-chosen status became a label value of pad_chat_commands_total")
	}
}

func labelValueSeen(t *testing.T, family, prefix string) bool {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if strings.HasPrefix(l.GetValue(), prefix) {
					return true
				}
			}
		}
	}
	return false
}

// clearActive deletes the pointer only if it still names the finishing stream: a newer turn that wrote its pointer
// between the read and the delete keeps it (L7).
func TestCommands_ClearActiveKeepsANewerTurnsPointer(t *testing.T) {
	c := redisreplica.New(t, 1)
	a := newCmdReplica(t, c.Replicas[0])
	older := &streamCtl{ownerID: "u1", streamID: "old", flowID: "f1", convKey: "k"}
	newer := &streamCtl{ownerID: "u1", streamID: "new", flowID: "f1", convKey: "k"}
	key := chatActiveKey.Key(conversationToken("u1", "f1", "k"))
	if !a.cluster.publishActive(older) {
		t.Fatal("setup: pointer not written")
	}
	// Right after clearActive reads the pointer, a newer turn of the same conversation replaces it.
	var swapped atomic.Bool
	c.Replicas[0].Client.AddHook(&afterGetHook{key: key, fn: func() {
		if swapped.CompareAndSwap(false, true) {
			val, _ := json.Marshal(activeRecord{V: 1, Kind: chatActiveKey.Name(), Stream: "new", Replica: "replica-9"})
			c.Redis.Set(key, string(val))
		}
	}})
	a.cluster.clearActive(older)
	if !swapped.Load() {
		t.Fatal("test setup: the read was not observed")
	}
	got, err := c.Redis.Get(key)
	if err != nil || !strings.Contains(got, `"new"`) {
		t.Fatalf("the newer turn's pointer after the older stream cleared = %q, %v; want it kept", got, err)
	}
	_ = newer
}

type afterGetHook struct {
	key string
	fn  func()
}

func (h *afterGetHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *afterGetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if cmd.Name() == "get" && len(cmd.Args()) > 1 && cmd.Args()[1] == h.key {
			h.fn()
		}
		return err
	}
}
func (h *afterGetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// Two replicas given the same PAD_INSTANCE_ID are still two replicas to each other (M3): a stop from one reaches the
// stream on the other instead of being taken for its own and answered ok without stopping anything.
func TestCommands_TwoReplicasWithTheSameInstanceIDStillReachEachOther(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newSameNameReplica(t, c.Replicas[0], "same-host"), newSameNameReplica(t, c.Replicas[1], "same-host")
	if a.cluster.instance == b.cluster.instance {
		t.Fatalf("both replicas are %q to Redis", a.cluster.instance)
	}
	if redisx.OriginInstance(a.cluster.instance) != "same-host" {
		t.Errorf("origin %q does not keep the human-facing instance id", a.cluster.instance)
	}
	id := startBlockedStream(t, a, "u1")
	snap, ok := a.resumeBackplane().Load(context.Background(), id)
	if !ok || snap.Replica != a.cluster.instance {
		t.Fatalf("snapshot replica = %q (%v), want the stream's origin %q", snap.Replica, ok, a.cluster.instance)
	}
	if err := b.CancelStreamAs(context.Background(), "u1", id); err != nil {
		t.Fatalf("CancelStreamAs on the same-id replica: %v", err)
	}
	ctl := waitStreamFinished(t, a, id)
	if ctl.errCode != ChatErrStopped {
		t.Errorf("stream ended with %q, want the stop recorded as %q: the stop was taken for this replica's own", ctl.errCode, ChatErrStopped)
	}
}

// newSameNameReplica is a ChatService wired the way NewChatService wires it, with the instance id a deployment
// configured (possibly the same on several replicas).
func newSameNameReplica(t *testing.T, r *redisreplica.Replica, instance string) *ChatService {
	t.Helper()
	ai.RegisterProvider("cmd-blocking", func(string) ai.Provider { return &cmdProvider{FakeProvider: &testutil.FakeProvider{}} })
	svc := &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
	}
	svc.attachCluster(r.Client, instance, redisx.DeriveBusKey("a-deployment-secret"))
	svc.StartCommands()
	t.Cleanup(svc.StopCommands)
	redisreplica.Eventually(t, 3*time.Second, r.ID+" command bus subscribed", svc.cluster.bus.Ready)
	return svc
}

// Commands are signed with the deployment's key (M2): a replica holding another key is not obeyed, and the stop it
// asked for does not happen.
func TestCommands_AReplicaWithAnotherKeyIsNotObeyed(t *testing.T) {
	c := redisreplica.New(t, 2)
	a := newSameNameReplica(t, c.Replicas[0], "host-a")
	b := &ChatService{notifier: &testutil.CountingNotifier{}}
	b.attachCluster(c.Replicas[1].Client, "host-b", redisx.DeriveBusKey("some-other-secret"))
	b.StartCommands()
	t.Cleanup(b.StopCommands)
	redisreplica.Eventually(t, 3*time.Second, "b subscribed", b.cluster.bus.Ready)
	prev := chatCommandWait
	chatCommandWait = 200 * time.Millisecond
	t.Cleanup(func() { chatCommandWait = prev })

	id := startBlockedStream(t, a, "u1")
	if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("stop signed with another key = %v, want ErrReplicaUnreachable", err)
	}
	time.Sleep(50 * time.Millisecond)
	if !streamRunning(a, id) {
		t.Fatal("the stream was stopped by a command that did not verify")
	}
}
