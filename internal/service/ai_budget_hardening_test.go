package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/testutil/redisreplica"
)

// The fix round of REVIEW_REDIS H1, L2 and M5 for the budget ledger: a slow or absent Redis must not queue every
// stream start behind one lock, a degraded Redis is not asked at all, a released stream does not come back, and an
// entry the server lost fails the next check closed.

// gatedLedger holds its first hold until gate closes: a Redis round trip that takes as long as the test wants.
type gatedLedger struct {
	memLedger
	gate    chan struct{}
	entered chan struct{}
	calls   atomic.Int32
}

func newGatedLedger() *gatedLedger {
	return &gatedLedger{gate: make(chan struct{}), entered: make(chan struct{})}
}

func (l *gatedLedger) hold(e inFlightSpend) error {
	if l.calls.Add(1) == 1 {
		close(l.entered)
		<-l.gate
	}
	return l.memLedger.hold(e)
}

type startResult struct {
	ok  bool
	err error
}

func startAsync(svc *ChatService, id, scope string) <-chan startResult {
	out := make(chan startResult, 1)
	go func() {
		ok, err := svc.tryStartStream(id, scope, &streamCtl{ownerID: scope, streamID: id, cancel: func() {}})
		out <- startResult{ok, err}
	}()
	return out
}

// One slow ledger write must not hold up the starts of other users: the cap lock covers the cap bookkeeping only.
func TestTryStartStream_ASlowLedgerDoesNotQueueOtherStarts(t *testing.T) {
	led := newGatedLedger()
	svc := &ChatService{budget: newAIBudgetWithLedger(nil, nil, led)}
	first := startAsync(svc, "s1", "u1")
	<-led.entered

	second := startAsync(svc, "s2", "u2")
	select {
	case r := <-second:
		if !r.ok || r.err != nil {
			t.Fatalf("second start = %+v, want admitted", r)
		}
	case <-time.After(time.Second):
		t.Fatal("a second stream start waited for the first one's ledger write: the Redis round trip is under the cap lock")
	}
	close(led.gate)
	if r := <-first; !r.ok || r.err != nil {
		t.Fatalf("first start = %+v, want admitted once the ledger answered", r)
	}
	// The per-caller cap still counts a start whose ledger write is in flight.
	if _, ok := svc.activeStreams.Load("s1"); !ok {
		t.Error("the admitted stream is not registered")
	}
}

// A stop must not wait on a slow Redis: Shutdown sets the closing flag and ends the workers without queueing behind
// a stream start that is waiting for the ledger; that start is then refused and leaves nothing behind.
func TestTryStartStream_ShutdownDoesNotWaitForALedgerWrite(t *testing.T) {
	led := newGatedLedger()
	svc := &ChatService{budget: newAIBudgetWithLedger(nil, nil, led), shutdownBudget: time.Second}
	first := startAsync(svc, "s1", "u1")
	<-led.entered

	done := make(chan error, 1)
	go func() { done <- svc.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown = %v, want a clean stop (the pending start counted no worker)", err)
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("Shutdown waited for a stream start's ledger write")
	}

	close(led.gate)
	r := <-first
	if r.ok || !errors.Is(r.err, ErrServerRestarting) {
		t.Fatalf("a start that finished its ledger write after Shutdown = %+v, want ErrServerRestarting", r)
	}
	if _, active := svc.activeStreams.Load("s1"); active {
		t.Error("the refused stream is still registered as active")
	}
	left := 0
	led.entries.Range(func(_, _ any) bool { left++; return true })
	if left != 0 {
		t.Errorf("the refused stream's ledger entry was left behind (%d entries)", left)
	}
	svc.workers.Wait() // nothing was counted
}

// holdFailingLedger cannot take a hold.
type holdFailingLedger struct{ memLedger }

func (l *holdFailingLedger) hold(inFlightSpend) error { return errors.New("boom") }

// A refused start (the ledger cannot answer) leaves no trace, whichever way it fails.
func TestTryStartStream_AFailedLedgerWriteReleasesTheSlotAndTheCap(t *testing.T) {
	led := &holdFailingLedger{}
	svc := &ChatService{budget: newAIBudgetWithLedger(nil, nil, led)}
	for i := range maxConcurrentStreamsPerScope + 2 { // more tries than the cap: a leaked slot would turn into cap refusals
		ok, err := svc.tryStartStream("s"+string(rune('a'+i)), "u1", &streamCtl{ownerID: "u1"})
		if ok || !errors.Is(err, ErrAIBudgetUnavailable) {
			t.Fatalf("try %d = (%v, %v), want (false, ErrAIBudgetUnavailable)", i, ok, err)
		}
	}
	svc.workers.Wait()
}

// With Redis marked degraded the ledger refuses without a round trip: the answer is fail closed anyway, and a
// network wait per stream start (2 s each) is what makes a Redis outage look like a hung server.
func TestRedisLedger_DegradedRedisFailsFastWithoutAskingIt(t *testing.T) {
	slowRefresh(t, time.Hour)
	c := redisreplica.New(t, 1)
	r := c.Replicas[0]
	l := newRedisLedger(r.Client, r.Health)
	for range 3 {
		r.Health.RecordError("test", errors.New("boom"))
	}
	if !r.Health.Degraded() {
		t.Fatal("test setup: Redis is not marked degraded")
	}
	before := c.Redis.CommandCount()
	started := time.Now()
	if err := l.hold(reservingStream("u1", "org-1", 0.2)); err == nil {
		t.Error("hold succeeded while Redis is degraded")
	}
	if _, _, err := l.others(context.Background(), "u1", "org-1", nil); err == nil {
		t.Error("others answered while Redis is degraded")
	}
	if took := time.Since(started); took > 100*time.Millisecond {
		t.Errorf("refusals took %v, want them immediate", took)
	}
	if n := c.Redis.CommandCount() - before; n != 0 {
		t.Errorf("a degraded ledger sent %d commands to Redis, want none", n)
	}
	// The error classifies like any other ledger failure: the budget answers AI_BUDGET_UNAVAILABLE.
	b := newAIBudgetWithLedger(&staticSettings{}, nil, l)
	if err := b.hold(reservingStream("u1", "", 0.2)); !errors.Is(err, ErrAIBudgetUnavailable) {
		t.Errorf("AIBudget.hold on a degraded ledger = %v, want ErrAIBudgetUnavailable", err)
	}
}

// The refresh rewrites a copy of the held set taken before it writes; a stream released in between must not be
// written back (it would count against the budget for a whole lease).
func TestRedisLedger_AReleasedStreamIsNotWrittenBackByARefreshInFlight(t *testing.T) {
	slowRefresh(t, time.Hour)
	c := redisreplica.New(t, 1)
	r := c.Replicas[0]
	l := newRedisLedger(r.Client, r.Health)
	ctl := reservingStream("u1", "org-1", 0.3)
	if err := l.hold(ctl); err != nil {
		t.Fatal(err)
	}
	// Play the refresh's two halves by hand: copy, then the stream ends, then the write lands.
	snapshot := l.entriesHeld()
	if err := l.release(ctl); err != nil {
		t.Fatal(err)
	}
	l.writeRefresh(snapshot)
	if keys, _ := c.Redis.HKeys(redisx.InflightOrgKey.Key("org-1")); len(keys) != 0 {
		t.Errorf("the released stream was resurrected by the refresh: %v", keys)
	}
	if keys, _ := c.Redis.HKeys(redisx.InflightUserKey.Key("u1")); len(keys) != 0 {
		t.Errorf("the released stream was resurrected in the user hash: %v", keys)
	}
}

// The server evicted a held stream's record (maxmemory) or it was pruned during a stall: the sums can no longer be
// trusted, so the next check on that replica refuses once, and counts it.
func TestRedisLedger_ALostEntryFailsTheNextCheckClosedOnce(t *testing.T) {
	slowRefresh(t, time.Hour)
	c := redisreplica.New(t, 2)
	a, b := replicaBudget(t, c.Replicas[0], 10, nil), replicaBudget(t, c.Replicas[1], 10, nil)

	mine, other := reservingStream("u1", "org-1", 0.2), reservingStream("u2", "org-1", 0.2)
	if err := admit(a, mine, 0.2); err != nil {
		t.Fatal(err)
	}
	if err := admit(b, other, 0.2); err != nil {
		t.Fatal(err)
	}
	// Healthy: checks pass and the held entry is not "lost".
	if err := a.check(context.Background(), nil, "u1", "org-1", 0, mine); err != nil {
		t.Fatalf("check on an intact ledger: %v", err)
	}
	lost := ledgerMetric(t, "pad_ai_ledger_errors_total", "lost")

	// maxmemory-policy evicts both hashes.
	c.Redis.Del(redisx.InflightUserKey.Key("u1"))
	c.Redis.Del(redisx.InflightOrgKey.Key("org-1"))

	err := a.check(context.Background(), nil, "u1", "org-1", 0, mine)
	if !errors.Is(err, ErrAIBudgetUnavailable) {
		t.Fatalf("check after the held entry vanished = %v, want ErrAIBudgetUnavailable (the sums are not trustworthy)", err)
	}
	if got := ledgerMetric(t, "pad_ai_ledger_errors_total", "lost"); got != lost+1 {
		t.Errorf("lost count = %v, want %v", got, lost+1)
	}
	// The check re-created the record; the next one is whole again.
	if err := a.check(context.Background(), nil, "u1", "org-1", 0, mine); err != nil {
		t.Errorf("second check after the entry was rewritten: %v, want it to pass again", err)
	}
}

// A refresh that finds its stream's record gone marks the same condition for the next check.
func TestRedisLedger_ARefreshThatRecreatesAnEntryMarksItLost(t *testing.T) {
	slowRefresh(t, time.Hour)
	c := redisreplica.New(t, 1)
	a := replicaBudget(t, c.Replicas[0], 10, nil)
	mine := reservingStream("u1", "org-1", 0.2)
	if err := admit(a, mine, 0.2); err != nil {
		t.Fatal(err)
	}
	c.Redis.Del(redisx.InflightUserKey.Key("u1"))
	c.Redis.Del(redisx.InflightOrgKey.Key("org-1"))
	l := a.ledger.(*redisLedger)
	l.writeRefresh(l.entriesHeld())
	if err := a.check(context.Background(), nil, "u1", "org-1", 0, mine); !errors.Is(err, ErrAIBudgetUnavailable) {
		t.Fatalf("check after a refresh had to re-create the entry = %v, want ErrAIBudgetUnavailable", err)
	}
	if err := a.check(context.Background(), nil, "u1", "org-1", 0, mine); err != nil {
		t.Errorf("the refusal repeated: %v", err)
	}
}
