package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/redisreplica"
	"pad-core/models"
)

// Two-replica scenarios of the stream commands (ADR 0033) on the redisreplica harness: the stream runs on one
// replica, the request lands on another.

// cmdProvider is a model that streams nothing until its stream is stopped.
type cmdProvider struct{ *testutil.FakeProvider }

func (p *cmdProvider) Stream(ctx context.Context, _ ai.Request, _ func(ai.Chunk)) error {
	<-ctx.Done()
	return ctx.Err()
}

// newCmdReplica is a ChatService wired to one harness replica as the DI wires it: the resume store and the cluster
// share the replica's client and instance name, and the command bus is subscribed.
func newCmdReplica(t *testing.T, r *redisreplica.Replica) *ChatService {
	t.Helper()
	ai.RegisterProvider("cmd-blocking", func(string) ai.Provider { return &cmdProvider{FakeProvider: &testutil.FakeProvider{}} })
	svc := &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
		resume:  redisResumeStore{c: r.Client, instance: r.ID},
		cluster: newChatCluster(r.Client, r.ID),
	}
	svc.StartCommands()
	t.Cleanup(svc.StopCommands)
	redisreplica.Eventually(t, 3*time.Second, r.ID+" command bus subscribed", svc.cluster.bus.Ready)
	return svc
}

// startBlockedStream starts a stream for scope on svc that waits for its stop, and returns its id once the snapshot
// naming svc as the owner is in Redis.
func startBlockedStream(t *testing.T, svc *ChatService, scope string) string {
	t.Helper()
	prev := resumeMirrorInterval
	resumeMirrorInterval = 20 * time.Millisecond
	t.Cleanup(func() { resumeMirrorInterval = prev })
	id := uuid.NewString()
	doc := &models.FlowDocument{ID: "f1"}
	if _, err := svc.StreamChatMessage(context.Background(), scope, doc, nil, models.ChatRequest{
		Provider: "cmd-blocking", Model: "fake-model", UserMessage: "hi", ClientStreamID: id,
	}); err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	// Registered after the interval's restore, so it runs before it: no mirror outlives the test.
	t.Cleanup(func() { svc.CancelStream(id); svc.workers.Wait() })
	redisreplica.Eventually(t, 3*time.Second, "the stream's snapshot in Redis", func() bool {
		_, ok := svc.resumeBackplane().Load(context.Background(), id)
		return ok
	})
	return id
}

func streamRunning(svc *ChatService, id string) bool {
	_, ok := svc.activeStreams.Load(id)
	return ok
}

// A stop that lands on the replica that does not run the stream reaches the one that does.
func TestCommands_CancelOnTheOtherReplica(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	originOK := promtest.ToFloat64(chatCommandsTotal.WithLabelValues("cancel", "origin", "ok"))
	ownerOK := promtest.ToFloat64(chatCommandsTotal.WithLabelValues("cancel", "owner", "ok"))

	if err := b.CancelStreamAs(context.Background(), "u1", id); err != nil {
		t.Fatalf("CancelStreamAs on the other replica: %v", err)
	}
	ctl := waitStreamFinished(t, a, id)
	if ctl.errCode != ChatErrStopped {
		t.Errorf("stream ended with %q, want the stop recorded as %q", ctl.errCode, ChatErrStopped)
	}
	if got := promtest.ToFloat64(chatCommandsTotal.WithLabelValues("cancel", "origin", "ok")) - originOK; got != 1 {
		t.Errorf("origin ok count += %v, want 1", got)
	}
	if got := promtest.ToFloat64(chatCommandsTotal.WithLabelValues("cancel", "owner", "ok")) - ownerOK; got != 1 {
		t.Errorf("owner ok count += %v, want 1", got)
	}
}

// The owner decides who may stop a stream: another user's stop changes nothing, whichever replica asked and
// whatever the asking replica checked.
func TestCommands_CancelByAnotherUserIsRefused(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")

	if err := b.CancelStreamAs(context.Background(), "u2", id); !errors.Is(err, ErrStreamNotOwned) {
		t.Fatalf("CancelStreamAs by u2 = %v, want ErrStreamNotOwned", err)
	}
	// An asking replica that skipped its own check (a bug there) is stopped by the owner's.
	status, err := b.cluster.call(context.Background(), "replica-1", chatCommand{Op: opCancel, StreamID: id, Scope: "u2"})
	if err != nil || status != replyForbidden {
		t.Fatalf("command from u2 = (%q, %v), want %q", status, err, replyForbidden)
	}
	time.Sleep(50 * time.Millisecond)
	if !streamRunning(a, id) {
		t.Fatal("the stream was stopped by a user who does not own it")
	}
}

// A stream that has ended is not an error to stop, wherever the stop lands.
func TestCommands_CancelAnEndedStreamIsANoOp(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	a.CancelStream(id)
	waitStreamFinished(t, a, id)
	redisreplica.Eventually(t, 3*time.Second, "the final snapshot", func() bool {
		snap, ok := a.resumeBackplane().Load(context.Background(), id)
		return ok && snap.Error != ""
	})
	if err := b.CancelStreamAs(context.Background(), "u1", id); err != nil {
		t.Fatalf("CancelStreamAs of an ended stream = %v, want nil", err)
	}
}

// An approval whose card the user answers on another replica is decided on the replica that waits for it, and the
// asking replica's check of the token's scope runs before anything is sent.
func TestCommands_DecisionOnTheOtherReplica(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	a.activeStreams.Store("s1", &streamCtl{ownerID: "u1", streamID: "s1"})
	type result struct {
		dec approvalDecision
		out approvalOutcome
	}
	done := make(chan result, 1)
	go func() {
		dec, out := a.requestApproval(context.Background(), "s1", approvalRequest{id: "p1", tool: "apply_fix", requiredScope: auth.ScopeWrite},
			func(string, map[string]interface{}) {}, nil)
		done <- result{dec, out}
	}()
	redisreplica.Eventually(t, 3*time.Second, "the pending approval in Redis", func() bool { return c.Redis.Exists(chatApprovalKey.Key("p1")) })
	if ttl := c.Redis.TTL(chatApprovalKey.Key("p1")); ttl <= 0 || ttl > chatApprovalKey.Spec().TTL {
		t.Errorf("approval key TTL = %v, want within the declared %v", ttl, chatApprovalKey.Spec().TTL)
	}

	// A token without the tool's scope is refused where it is held, and the approval stays pending.
	chatOnly := auth.WithActor(context.Background(), &auth.Actor{Kind: auth.ActorToken, UserID: "u1", Scopes: []string{auth.ScopeChat}})
	if err := b.DecideApproval(chatOnly, "u1", "s1", "p1", true, nil); !errors.Is(err, ErrScopeDenied) {
		t.Fatalf("decision with a chat-only token = %v, want ErrScopeDenied", err)
	}
	// The owner repeats the check for a command that arrives without it.
	if status, err := b.cluster.call(context.Background(), "replica-1", chatCommand{Op: opDecide, StreamID: "s1", Scope: "u1", ProposalID: "p1", Approved: true, TokenScopes: []string{auth.ScopeChat}}); err != nil || status != replyScopeDenied {
		t.Fatalf("command with a chat-only token = (%q, %v), want %q", status, err, replyScopeDenied)
	}
	// ... and the stream's owner.
	if status, err := b.cluster.call(context.Background(), "replica-1", chatCommand{Op: opDecide, StreamID: "s1", Scope: "u2", ProposalID: "p1", Approved: true}); err != nil || status != replyForbidden {
		t.Fatalf("command from another user = (%q, %v), want %q", status, err, replyForbidden)
	}
	// Another stream's id does not find the approval.
	if err := b.DecideApproval(context.Background(), "u1", "s2", "p1", true, nil); !errors.Is(err, ErrNoPendingFix) {
		t.Fatalf("decision naming another stream = %v, want ErrNoPendingFix", err)
	}
	select {
	case r := <-done:
		t.Fatalf("the approval resolved without a valid decision: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	writer := auth.WithActor(context.Background(), &auth.Actor{Kind: auth.ActorToken, UserID: "u1", Scopes: []string{auth.ScopeChat, auth.ScopeWrite}})
	if err := b.DecideApproval(writer, "u1", "s1", "p1", true, []int{2}); err != nil {
		t.Fatalf("decision on the other replica: %v", err)
	}
	r := <-done
	if r.out != approvalDecided || !r.dec.approved || len(r.dec.excluded) != 1 || r.dec.excluded[0] != 2 {
		t.Fatalf("approval = %+v, want approved with item 2 opted out", r)
	}
	redisreplica.Eventually(t, 3*time.Second, "the pending approval removed", func() bool { return !c.Redis.Exists(chatApprovalKey.Key("p1")) })
	// Decided: a repeat finds nothing pending, as it does on the owner.
	if err := b.DecideApproval(writer, "u1", "s1", "p1", true, nil); !errors.Is(err, ErrNoPendingFix) {
		t.Fatalf("repeat decision = %v, want ErrNoPendingFix", err)
	}
}

// Two replicas decide the same approval at once: one decision reaches the owner, the other is dropped.
func TestCommands_FirstDecisionFromAnyReplicaWins(t *testing.T) {
	c := redisreplica.New(t, 3)
	a := newCmdReplica(t, c.Replicas[0])
	b, d := newCmdReplica(t, c.Replicas[1]), newCmdReplica(t, c.Replicas[2])
	a.activeStreams.Store("s1", &streamCtl{ownerID: "u1", streamID: "s1"})
	done := make(chan approvalDecision, 1)
	go func() {
		dec, _ := a.requestApproval(context.Background(), "s1", approvalRequest{id: "p1", tool: "apply_fix"}, func(string, map[string]interface{}) {}, nil)
		done <- dec
	}()
	redisreplica.Eventually(t, 3*time.Second, "the pending approval in Redis", func() bool { return c.Redis.Exists(chatApprovalKey.Key("p1")) })
	// Hold the claim as if replica-2's decision were in flight, then replica-3 decides the opposite. Replica-3 waits for
	// replica-2's outcome (a loser's ok must mean the decision reached the owner) and answers with it.
	won, claim, err := b.cluster.claimDecision(context.Background(), "p1")
	if err != nil || !won {
		t.Fatalf("first claim = (%v, %v), want it won", won, err)
	}
	second := make(chan error, 1)
	go func() { second <- d.DecideApproval(context.Background(), "u1", "s1", "p1", false, nil) }()
	time.Sleep(50 * time.Millisecond)
	b.cluster.finishDecision("p1", claim, replyOK) // replica-2's decision reached the owner (not modelled here)
	if err := <-second; err != nil {
		t.Fatalf("second decision: %v, want an ok that sent nothing", err)
	}
	select {
	case dec := <-done:
		t.Fatalf("the owner received %+v from the replica that lost the claim", dec)
	case <-time.After(100 * time.Millisecond):
	}
}

// Racing decisions from two replicas: the owner is sent one of them.
func TestCommands_ConcurrentDecisionsReachTheOwnerOnce(t *testing.T) {
	c := redisreplica.New(t, 3)
	a := newCmdReplica(t, c.Replicas[0])
	b, d := newCmdReplica(t, c.Replicas[1]), newCmdReplica(t, c.Replicas[2])
	a.activeStreams.Store("s1", &streamCtl{ownerID: "u1", streamID: "s1"})
	done := make(chan approvalDecision, 1)
	go func() {
		dec, _ := a.requestApproval(context.Background(), "s1", approvalRequest{id: "p1", tool: "apply_fix"}, func(string, map[string]interface{}) {}, nil)
		done <- dec
	}()
	redisreplica.Eventually(t, 3*time.Second, "the pending approval in Redis", func() bool { return c.Redis.Exists(chatApprovalKey.Key("p1")) })
	before := promtest.ToFloat64(chatCommandsTotal.WithLabelValues("decide", "owner", "ok"))

	var wg sync.WaitGroup
	for _, r := range []struct {
		svc      *ChatService
		approved bool
	}{{b, true}, {d, false}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.svc.DecideApproval(context.Background(), "u1", "s1", "p1", r.approved, nil); err != nil && !errors.Is(err, ErrNoPendingFix) {
				t.Errorf("decision: %v", err)
			}
		}()
	}
	wg.Wait()
	<-done
	time.Sleep(50 * time.Millisecond)
	if got := promtest.ToFloat64(chatCommandsTotal.WithLabelValues("decide", "owner", "ok")) - before; got != 1 {
		t.Errorf("the owner acted on %v decisions, want exactly 1", got)
	}
}

// The replica that runs the stream is gone: the stop and the decision are refused at once as retryable, and the
// decision can be sent again once the replica listens.
func TestCommands_OwnerNotListening(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	a.activeStreams.Store("s9", &streamCtl{ownerID: "u1", streamID: "s9"})
	done := make(chan approvalDecision, 1)
	go func() {
		dec, _ := a.requestApproval(context.Background(), "s9", approvalRequest{id: "p9", tool: "apply_fix"}, func(string, map[string]interface{}) {}, nil)
		done <- dec
	}()
	redisreplica.Eventually(t, 3*time.Second, "the pending approval in Redis", func() bool { return c.Redis.Exists(chatApprovalKey.Key("p9")) })

	a.StopCommands() // the process is gone as far as the others can tell
	redisreplica.Eventually(t, 3*time.Second, "replica-1's subscription gone", func() bool {
		return c.Redis.PubSubNumSub(chatCmdChannel.Key("replica-1"))[chatCmdChannel.Key("replica-1")] == 0
	})
	started := time.Now()
	if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("CancelStreamAs with the owner gone = %v, want ErrReplicaUnreachable", err)
	}
	if err := b.DecideApproval(context.Background(), "u1", "s9", "p9", true, nil); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("DecideApproval with the owner gone = %v, want ErrReplicaUnreachable", err)
	}
	if took := time.Since(started); took > chatCommandWait/2 {
		t.Errorf("refusals took %v: an owner that does not listen must be known at once, not after the wait", took)
	}
	if !streamRunning(a, id) {
		t.Fatal("a refused stop stopped the stream anyway")
	}

	// The failed decision gave its claim back: back up, the same decision goes through.
	a.StartCommands()
	redisreplica.Eventually(t, 3*time.Second, "replica-1 subscribed again", a.cluster.bus.Ready)
	if err := b.DecideApproval(context.Background(), "u1", "s9", "p9", true, nil); err != nil {
		t.Fatalf("decision after the owner is back: %v", err)
	}
	if dec := <-done; !dec.approved {
		t.Errorf("owner received %+v, want the approval", dec)
	}
}

// An owner that is listening but does not answer in time is also unreachable, and the same retry works.
func TestCommands_OwnerDoesNotAnswer(t *testing.T) {
	prev := chatCommandWait
	chatCommandWait = 150 * time.Millisecond
	t.Cleanup(func() { chatCommandWait = prev })
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	// Replace the owner's handler with one that never answers.
	a.StopCommands()
	a.cluster.bus.Start(func(context.Context, []byte) []byte { return nil })
	redisreplica.Eventually(t, 3*time.Second, "replica-1 subscribed again", a.cluster.bus.Ready)

	if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("CancelStreamAs of a silent owner = %v, want ErrReplicaUnreachable", err)
	}
}

// Redis down: a stop or a decision for a stream another replica runs says so (503) instead of an ok; the stream's
// own replica keeps working without Redis; when Redis is back the commands cross again.
func TestCommands_RedisDown(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	a.activeStreams.Store("s1", &streamCtl{ownerID: "u1", streamID: "s1"})
	local := make(chan approvalDecision, 1)
	remote := make(chan approvalDecision, 1)
	go func() {
		dec, _ := a.requestApproval(context.Background(), "s1", approvalRequest{id: "p1", tool: "apply_fix"}, func(string, map[string]interface{}) {}, nil)
		remote <- dec
	}()
	redisreplica.Eventually(t, 3*time.Second, "the pending approval in Redis", func() bool { return c.Redis.Exists(chatApprovalKey.Key("p1")) })

	c.Kill()
	if _, err := b.OwnerOfChecked(context.Background(), id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Errorf("OwnerOfChecked with Redis down = %v, want ErrReplicaUnreachable", err)
	}
	if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Errorf("CancelStreamAs with Redis down = %v, want ErrReplicaUnreachable (never a false ok)", err)
	}
	if err := b.DecideApproval(context.Background(), "u1", "s1", "p1", true, nil); !errors.Is(err, ErrReplicaUnreachable) {
		t.Errorf("DecideApproval with Redis down = %v, want ErrReplicaUnreachable", err)
	}
	if !streamRunning(a, id) {
		t.Fatal("the stream ended although no command reached it")
	}

	// The owner's own replica needs no Redis for either.
	a.activeStreams.Store("s2", &streamCtl{ownerID: "u1", streamID: "s2"})
	go func() {
		dec, _ := a.requestApproval(context.Background(), "s2", approvalRequest{id: "p2", tool: "apply_fix"}, func(string, map[string]interface{}) {}, nil)
		local <- dec
	}()
	redisreplica.Eventually(t, 3*time.Second, "the approval pending locally", func() bool { _, ok := a.pendingFixDecisions.Load("p2"); return ok })
	if err := a.DecideApproval(context.Background(), "u1", "s2", "p2", true, nil); err != nil {
		t.Fatalf("local decision with Redis down: %v", err)
	}
	if dec := <-local; !dec.approved {
		t.Errorf("local approval = %+v", dec)
	}
	if err := a.CancelStreamAs(context.Background(), "u1", id); err != nil {
		t.Fatalf("local stop with Redis down: %v", err)
	}
	waitStreamFinished(t, a, id)

	// Back: the subscriptions re-establish by themselves and the first approval can be decided from the other replica.
	c.Restore(t)
	redisreplica.Eventually(t, 10*time.Second, "both buses subscribed again", func() bool { return a.cluster.bus.Ready() && b.cluster.bus.Ready() })
	// p1's key was pending in the restored data (the miniredis kept it).
	redisreplica.Eventually(t, 5*time.Second, "the decision crossing after Redis returned", func() bool {
		return b.DecideApproval(context.Background(), "u1", "s1", "p1", true, nil) == nil
	})
	if dec := <-remote; !dec.approved {
		t.Errorf("approval decided after the outage = %+v", dec)
	}
}

// The approval's pending key is a courtesy: when it could not be written the approval still works for the replica
// that holds it, and a decision from elsewhere is told there is nothing pending.
func TestCommands_ApprovalKeyWriteFailureDoesNotBreakLocalDecisions(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	a.activeStreams.Store("s1", &streamCtl{ownerID: "u1", streamID: "s1"})
	c.Redis.SetError("READONLY simulated")
	shown := make(chan struct{})
	done := make(chan approvalDecision, 1)
	go func() {
		dec, _ := a.requestApproval(context.Background(), "s1", approvalRequest{id: "p1", tool: "apply_fix"}, func(string, map[string]interface{}) { close(shown) }, nil)
		done <- dec
	}()
	<-shown // the card is shown after the pending key was attempted
	c.Redis.SetError("")
	if c.Redis.Exists(chatApprovalKey.Key("p1")) {
		t.Fatal("the failed write left a key")
	}
	if err := b.DecideApproval(context.Background(), "u1", "s1", "p1", true, nil); !errors.Is(err, ErrNoPendingFix) {
		t.Fatalf("decision from the other replica = %v, want ErrNoPendingFix (the key never existed)", err)
	}
	if err := a.DecideApproval(context.Background(), "u1", "s1", "p1", false, nil); err != nil {
		t.Fatalf("local decision: %v", err)
	}
	if dec := <-done; dec.approved {
		t.Errorf("decision = %+v, want the decline", dec)
	}
}

// A reload that lands on the other replica finds the stream the first one runs.
func TestCommands_ActiveStreamIsFoundFromTheOtherReplica(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := newCmdReplica(t, c.Replicas[0]), newCmdReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	redisreplica.Eventually(t, 3*time.Second, "the question stored", func() bool {
		v, ok := a.activeStreams.Load(id)
		return ok && v.(*streamCtl).userTurnStored.Load()
	})
	var ctl *streamCtl
	if v, ok := a.activeStreams.Load(id); ok {
		ctl = v.(*streamCtl)
	}
	redisreplica.Eventually(t, 3*time.Second, "the conversation's pointer in Redis", func() bool {
		return c.Redis.Exists(chatActiveKey.Key(conversationToken("u1", ctl.flowID, ctl.convKey)))
	})
	if ttl := c.Redis.TTL(chatActiveKey.Key(conversationToken("u1", ctl.flowID, ctl.convKey))); ttl <= 0 || ttl > chatActiveKey.Spec().TTL {
		t.Errorf("pointer TTL = %v, want within the declared %v", ttl, chatActiveKey.Spec().TTL)
	}

	read, err := b.ReadConversation(context.Background(), "u1", ctl.flowID, ctl.convKey)
	if err != nil {
		t.Fatalf("ReadConversation on the other replica: %v", err)
	}
	if read.ActiveStreamID != id {
		t.Errorf("activeStreamId = %q, want %q", read.ActiveStreamID, id)
	}
	// Another user on the same conversation does not see it.
	if other, err := b.ReadConversation(context.Background(), "u2", ctl.flowID, ctl.convKey); err != nil || other.ActiveStreamID != "" {
		t.Errorf("another user's read = (%q, %v), want no stream", other.ActiveStreamID, err)
	}

	// Once the stream is over and its answer settled the pointer is gone, and a reload no longer re-attaches.
	a.CancelStream(id)
	waitStreamFinished(t, a, id)
	redisreplica.Eventually(t, 5*time.Second, "the pointer cleared", func() bool {
		return !c.Redis.Exists(chatActiveKey.Key(conversationToken("u1", ctl.flowID, ctl.convKey)))
	})
	if read, err = b.ReadConversation(context.Background(), "u1", ctl.flowID, ctl.convKey); err != nil || read.ActiveStreamID != "" {
		t.Errorf("after the stream, activeStreamId = (%q, %v), want none", read.ActiveStreamID, err)
	}
}

// Redis down: a reload on the other replica finds no stream (as before the pointer), and does not fail.
func TestCommands_ActiveStreamLookupFailsOpen(t *testing.T) {
	c := redisreplica.New(t, 2)
	b := newCmdReplica(t, c.Replicas[1])
	c.Kill()
	read, err := b.ReadConversation(context.Background(), "u1", "f1", "default")
	if err != nil || read.ActiveStreamID != "" {
		t.Fatalf("ReadConversation with Redis down = (%q, %v), want no stream and no error", read.ActiveStreamID, err)
	}
}

// The snapshot names its replica, carries the envelope, and the owner is what a command is routed by.
func TestCommands_SnapshotNamesTheOwnerReplica(t *testing.T) {
	c := redisreplica.New(t, 2)
	a := newCmdReplica(t, c.Replicas[0])
	id := startBlockedStream(t, a, "u1")
	snap, found, err := redisResumeStore{c: c.Replicas[1].Client}.Lookup(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("lookup = (%v, %v)", found, err)
	}
	if snap.Replica != "replica-1" || snap.V != 1 || snap.Kind != "chat.resume" || snap.Owner != "u1" {
		t.Errorf("snapshot = %+v, want replica-1, v1, chat.resume, owner u1", snap)
	}
}

// Without Redis nothing about the stop changes: a stream that is not here is not anywhere.
func TestCommands_WithoutRedisNothingChanges(t *testing.T) {
	svc := &ChatService{}
	if err := svc.CancelStreamAs(context.Background(), "u1", "unknown"); err != nil {
		t.Errorf("CancelStreamAs without Redis = %v, want nil as before", err)
	}
	if err := svc.DecideApproval(context.Background(), "u1", "s", "p", true, nil); !errors.Is(err, ErrNoPendingFix) {
		t.Errorf("DecideApproval without Redis = %v, want ErrNoPendingFix as before", err)
	}
	if owner, err := svc.OwnerOfChecked(context.Background(), "unknown"); err != nil || owner != "" {
		t.Errorf("OwnerOfChecked without Redis = (%q, %v)", owner, err)
	}
	svc.StartCommands() // no-ops
	svc.StopCommands()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		svc.cluster.registerApproval("p", "s", "", time.Second)
		svc.cluster.dropApproval("p")
	}()
	wg.Wait()
}

// The stop of the server ends the command subscriptions after the streams (they answer relayed stops while the
// streams wind down), and a replica that stopped answers no more.
func TestCommands_ShutdownEndsTheSubscriptions(t *testing.T) {
	c := redisreplica.New(t, 1)
	a := newCmdReplica(t, c.Replicas[0])
	startBlockedStream(t, a, "u1")
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if a.cluster.bus.Ready() {
		t.Error("the command bus still runs after Shutdown")
	}
	redisreplica.Eventually(t, 3*time.Second, "replica-1's subscription gone", func() bool {
		return c.Redis.PubSubNumSub(chatCmdChannel.Key("replica-1"))[chatCmdChannel.Key("replica-1")] == 0
	})
}
