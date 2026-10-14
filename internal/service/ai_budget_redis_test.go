package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/redisx"
	tu "pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/redisreplica"
	"pad-core/models"
)

// Phase 5b, the AI budget's in-flight ledger over Redis (SURVEY_REALTIME 8.4, ADR 0028 addendum): two replicas
// that share a Redis count each other's running streams, a replica that dies stops counting after its lease, and
// with Redis down a configured budget refuses (fail closed) while an unconfigured one never notices.

// ledgerClock is a clock the test advances.
type ledgerClock struct{ ns atomic.Int64 }

func newLedgerClock() *ledgerClock {
	c := &ledgerClock{}
	c.ns.Store(time.Now().UnixNano())
	return c
}
func (c *ledgerClock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *ledgerClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// replicaBudget is one replica's AIBudget on the shared Redis: its own client, its own ledger, the shared usage
// store and the deployment's daily budget.
func replicaBudget(t testing.TB, r *redisreplica.Replica, daily float64, clock *ledgerClock) *AIBudget {
	t.Helper()
	settings := &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: daily}}}
	b := newAIBudgetWithLedger(settings, tu.NewFakeBackend(), newRedisLedgerClock(r, clock))
	b.shared = true
	return b
}

func newRedisLedgerClock(r *redisreplica.Replica, clock *ledgerClock) *redisLedger {
	l := newRedisLedger(r.Client, r.Health)
	if clock != nil {
		l.led.WithClock(clock.Now)
	}
	return l
}

// reservingStream is a stream that has reserved cost for its next response.
func reservingStream(owner, org string, cost float64) *streamCtl {
	ctl := &streamCtl{ownerID: owner, orgID: org}
	ctl.setInflight(cost)
	return ctl
}

// admit is what a turn does at its start: enter the ledger with nothing in flight (tryStartStream), reserve one
// response (prepareTurn's setInflight), then check the budget with the stream's own entry excluded.
func admit(b *AIBudget, ctl *streamCtl, reserve float64) error {
	if err := b.hold(ctl); err != nil {
		return err
	}
	ctl.setInflight(reserve)
	return b.check(context.Background(), nil, ctl.ownerID, ctl.orgID, 0, ctl)
}

// slowRefresh keeps the background refresh out of a test that drives time by hand.
func slowRefresh(t *testing.T, d time.Duration) {
	t.Helper()
	old := ledgerRefreshEvery
	ledgerRefreshEvery = d
	t.Cleanup(func() { ledgerRefreshEvery = old })
}

func TestRedisLedger_ReplicasCountEachOthersStreams(t *testing.T) {
	slowRefresh(t, time.Hour)
	c := redisreplica.New(t, 2)
	a, b := replicaBudget(t, c.Replicas[0], 1.0, nil), replicaBudget(t, c.Replicas[1], 1.0, nil)

	// Replica A runs two of the org's streams ($0.40 each in flight); B's budget check sees them.
	s1, s2 := &streamCtl{ownerID: "u1", orgID: "org-1"}, &streamCtl{ownerID: "u2", orgID: "org-1"}
	for _, s := range []*streamCtl{s1, s2} {
		if err := admit(a, s, 0.4); err != nil {
			t.Fatalf("admit on A: %v", err)
		}
	}
	orgSum, userSum := b.othersInFlight("u3", "org-1", nil)
	if orgSum < 0.8-1e-9 || orgSum > 0.8+1e-9 {
		t.Errorf("B sees $%.2f in flight for the org, want $0.80 (A's two streams)", orgSum)
	}
	if userSum != 0 {
		t.Errorf("B sees $%.2f in flight for u3 personally, want 0", userSum)
	}
	// u1's personal sum is u1's streams on every replica, org or not.
	if _, userSum := b.othersInFlight("u1", "", nil); userSum < 0.4-1e-9 || userSum > 0.4+1e-9 {
		t.Errorf("B sees $%.2f in flight for u1, want $0.40", userSum)
	}
	// A third stream on B passes (0.8 < 1) and A then sees it: the fourth is refused wherever it lands.
	if err := admit(b, &streamCtl{ownerID: "u3", orgID: "org-1"}, 0.4); err != nil {
		t.Fatalf("third stream: %v", err)
	}
	if err := admit(a, &streamCtl{ownerID: "u4", orgID: "org-1"}, 0.4); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Fatalf("fourth stream (1.2 in flight): %v, want ErrAIBudgetExceeded", err)
	}
	// Releasing frees the budget on both replicas.
	a.release(s1)
	a.release(s2)
	if err := b.CheckBudget(context.Background(), "u9", "org-1"); err != nil {
		t.Errorf("after A's streams ended: %v", err)
	}
}

// The documented bound: a stream is admitted while the others' reservations are under the budget, so the sum of
// admitted reservations is under budget + one reservation, exactly what one replica's in-process ledger allows
// and no matter how the starts interleave across replicas (the publish and the read are one ordered round trip).
func TestRedisLedger_TwoReplicasCannotOvershootByMoreThanOneReservation(t *testing.T) {
	slowRefresh(t, time.Hour)
	const budget, reserve, attempts = 1.0, 0.4, 24
	c := redisreplica.New(t, 2)
	replicas := []*AIBudget{replicaBudget(t, c.Replicas[0], budget, nil), replicaBudget(t, c.Replicas[1], budget, nil)}

	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctl := &streamCtl{ownerID: fmt.Sprintf("u%d", i), orgID: "org-1"}
			<-start
			err := admit(replicas[i%2], ctl, reserve)
			if err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrAIBudgetExceeded) {
				t.Errorf("attempt %d: %v", i, err)
			}
			if err != nil {
				replicas[i%2].release(ctl) // a refused turn leaves the ledger as its worker's cleanup does
			}
		}()
	}
	close(start)
	wg.Wait()
	// others < 1.0 admits: at most ceil(1.0/0.4) = 3 reservations (1.2 total) can be admitted. (A stampede may
	// admit fewer, even none: every start saw all the others. That costs a retry, never budget.)
	if got := int(admitted.Load()); got > 3 {
		t.Fatalf("admitted %d streams of reservation $%.2f against a $%.2f budget, want at most 3 (budget + one reservation)", got, reserve, budget)
	}
	if spent := float64(admitted.Load()) * reserve; spent >= budget+reserve {
		t.Errorf("admitted reservations total $%.2f, over budget + one reservation", spent)
	}

	// The same race on two in-process ledgers (the single-replica code run twice) admits far more: that is the
	// overshoot the shared ledger removes.
	local := []*AIBudget{
		newAIBudgetWithLedger(&staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: budget}}}, tu.NewFakeBackend(), &memLedger{}),
		newAIBudgetWithLedger(&staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: budget}}}, tu.NewFakeBackend(), &memLedger{}),
	}
	split := 0
	for i := 0; i < 8; i++ {
		if admit(local[i%2], &streamCtl{ownerID: fmt.Sprintf("v%d", i), orgID: "org-1"}, reserve) == nil {
			split++
		}
	}
	if split <= 3 {
		t.Errorf("control: two separate ledgers admitted %d, expected more than the shared bound of 3", split)
	}
}

func TestRedisLedger_CrashedReplicasEntriesAgeOut(t *testing.T) {
	slowRefresh(t, time.Hour) // the "crashed" replica never refreshes again
	clock := newLedgerClock()
	c := redisreplica.New(t, 2)
	dead, live := replicaBudget(t, c.Replicas[1], 0.5, clock), replicaBudget(t, c.Replicas[0], 0.5, clock)

	orphan := reservingStream("u1", "org-1", 0.9)
	if err := dead.hold(orphan); err != nil {
		t.Fatal(err)
	}
	if err := live.CheckBudget(context.Background(), "u2", "org-1"); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Fatalf("with the other replica's $0.90 stream in flight: %v, want ErrAIBudgetExceeded", err)
	}
	key := redisx.InflightOrgKey.Key("org-1")
	if ttl := c.Redis.TTL(key); ttl <= 0 || ttl > 11*time.Minute {
		t.Errorf("org hash TTL = %v, want 0 < ttl <= 11m (every key expires)", ttl)
	}
	if ttl := c.Redis.TTL(redisx.InflightUserKey.Key("u1")); ttl <= 0 || ttl > 11*time.Minute {
		t.Errorf("user hash TTL = %v, want 0 < ttl <= 11m", ttl)
	}

	// Within the lease it still counts; past it the orphan is ignored and pruned. Nobody released anything.
	clock.Advance(20 * time.Second)
	if err := live.CheckBudget(context.Background(), "u2", "org-1"); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Fatalf("20 s after the last write: %v, want it still counted", err)
	}
	before := ledgerMetric(t, "pad_ai_ledger_expired_total", "")
	clock.Advance(15 * time.Second)
	if err := live.CheckBudget(context.Background(), "u2", "org-1"); err != nil {
		t.Fatalf("35 s after the last write: %v, want the orphan to have aged out", err)
	}
	if keys, _ := c.Redis.HKeys(key); len(keys) != 0 {
		t.Errorf("the expired orphan is still in the org hash: %v", keys)
	}
	if ledgerMetric(t, "pad_ai_ledger_expired_total", "")-before < 1 {
		t.Error("pad_ai_ledger_expired_total did not count the orphan")
	}
}

// ledgerMetric reads a pad_ai_ledger_* family from the metrics registry: the counter with the given label value
// (op, "" for none), or the gauge.
func ledgerMetric(t testing.TB, name, op string) float64 {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if op != "" && (len(m.GetLabel()) == 0 || m.GetLabel()[0].GetValue() != op) {
				continue
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	return 0
}

func aiBudgetCall(scope, org string) ai.BudgetCall {
	return ai.BudgetCall{Scope: scope, OrgID: org, ProviderID: "openai", Model: "m", Kind: ai.BudgetCallChat}
}

func TestRedisLedger_RefreshKeepsAGrowingStreamVisible(t *testing.T) {
	slowRefresh(t, 15*time.Millisecond)
	c := redisreplica.New(t, 2)
	a, b := replicaBudget(t, c.Replicas[0], 10, nil), replicaBudget(t, c.Replicas[1], 10, nil)

	ctl := reservingStream("u1", "org-1", 0.1)
	if err := a.hold(ctl); err != nil {
		t.Fatal(err)
	}
	// The stream spends: its in-flight cost grows between checks; the one-second mirror carries it to the others.
	ctl.setInflight(0.7)
	redisreplica.Eventually(t, 2*time.Second, "B to see the grown cost", func() bool {
		org, _ := b.othersInFlight("u2", "org-1", nil)
		return org > 0.69
	})
	a.release(ctl)
	redisreplica.Eventually(t, 2*time.Second, "the refresh goroutine to end with the last stream", func() bool {
		l := a.ledger.(*redisLedger)
		l.mu.Lock()
		defer l.mu.Unlock()
		return !l.running
	})
	if org, _ := b.othersInFlight("u2", "org-1", nil); org != 0 {
		t.Errorf("released stream still counts $%.2f", org)
	}
}

func TestRedisLedger_RedisDownFailsClosedWithABudget(t *testing.T) {
	slowRefresh(t, time.Hour)
	c := redisreplica.New(t, 1)
	b := replicaBudget(t, c.Replicas[0], 5, nil)
	svc := &ChatService{backend: tu.NewFakeBackend(), budget: b, settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 5}}}}

	running := reservingStream("u1", "org-1", 0.2)
	if err := b.hold(running); err != nil {
		t.Fatal(err)
	}
	holdErrs, readErrs := ledgerMetric(t, "pad_ai_ledger_errors_total", "hold"), ledgerMetric(t, "pad_ai_ledger_errors_total", "read")

	c.Kill()

	// A new stream is refused at admission and leaves no trace: not active, no worker counted.
	ok, err := svc.tryStartStream("s-new", "u2", &streamCtl{ownerID: "u2", orgID: "org-1"})
	if ok || !errors.Is(err, ErrAIBudgetUnavailable) {
		t.Fatalf("tryStartStream with Redis down = (%v, %v), want (false, ErrAIBudgetUnavailable)", ok, err)
	}
	if _, active := svc.activeStreams.Load("s-new"); active {
		t.Error("the refused stream is still registered as active")
	}
	if svc.closing {
		t.Fatal("test setup")
	}
	svc.workers.Wait() // returns at once: nothing was counted

	// The other entry points refuse the same way: the start check, the loop re-check's budget call, the hook.
	if err := b.CheckBudget(context.Background(), "u2", "org-1"); !errors.Is(err, ErrAIBudgetUnavailable) {
		t.Errorf("CheckBudget: %v", err)
	}
	if err := b.Check(withStreamSpend(context.Background(), running), aiBudgetCall("u1", "org-1")); !errors.Is(err, ErrAIBudgetUnavailable) {
		t.Errorf("hook Check: %v", err)
	}

	// A RUNNING stream stops at its next loop re-check, with the budget code and a message that says what is
	// wrong; there is no grace period.
	var gotCode, gotMsg string
	st := &toolLoopState{
		s: svc, ctx: withStreamSpend(context.Background(), running), scope: "u1", orgID: "org-1", ctl: running,
		provider: &tu.FakeProvider{}, awaitStart: func() bool { return true },
		emit: func(typ string, data map[string]interface{}) {
			if typ == "error" {
				gotCode, _ = data["code"].(string)
				gotMsg, _ = data["message"].(string)
			}
		},
	}
	if st.recheckBudget() {
		t.Fatal("the loop re-check passed with the ledger unreachable")
	}
	if gotCode != ChatErrBudgetUnavailable {
		t.Errorf("error code = %q, want %q", gotCode, ChatErrBudgetUnavailable)
	}
	if !strings.HasPrefix(gotMsg, "response stopped: ") || !strings.Contains(gotMsg, "AI budget") || strings.Contains(strings.ToLower(gotMsg), "redis") || strings.Contains(gotMsg, "127.0.0.1") {
		t.Errorf("error message = %q, want a clear client-safe budget message", gotMsg)
	}
	// Releasing is best effort: it does not panic or block, and the entry is left to age out.
	b.release(running)
	if got := ledgerMetric(t, "pad_ai_ledger_errors_total", "hold"); got < holdErrs+1 {
		t.Errorf("pad_ai_ledger_errors_total{op=hold} = %v, want it to count the refused hold (was %v)", got, holdErrs)
	}
	if got := ledgerMetric(t, "pad_ai_ledger_errors_total", "read"); got < readErrs+3 {
		t.Errorf("pad_ai_ledger_errors_total{op=read} = %v, want the three refused reads counted (was %v)", got, readErrs)
	}
	if got := ledgerMetric(t, "pad_ai_ledger_errors_total", "release"); got < 1 {
		t.Errorf("pad_ai_ledger_errors_total{op=release} = %v, want the failed release counted", got)
	}

	// Redis returns: the budget answers again without a restart.
	c.Restore(t)
	redisreplica.Eventually(t, 3*time.Second, "the budget to answer again", func() bool {
		_ = c.Replicas[0].Health.Ping(context.Background()) // the health loop's ping: a degraded ledger sends nothing itself
		return b.CheckBudget(context.Background(), "u2", "org-1") == nil
	})
}

func TestRedisLedger_NoBudgetNeverTouchesRedis(t *testing.T) {
	slowRefresh(t, 10*time.Millisecond)
	c := redisreplica.New(t, 1)
	b := replicaBudget(t, c.Replicas[0], 0, nil) // no daily budget
	c.Kill()                                     // and no Redis

	ctl := reservingStream("u1", "org-1", 0.3)
	if err := admit(b, ctl, 0.3); err != nil {
		t.Fatalf("a deployment without a daily budget must not depend on Redis: %v", err)
	}
	if err := b.CheckBudget(context.Background(), "u1", "org-1"); err != nil {
		t.Errorf("CheckBudget: %v", err)
	}
	if err := b.Check(withStreamSpend(context.Background(), ctl), aiBudgetCall("u1", "org-1")); err != nil {
		t.Errorf("hook Check: %v", err)
	}
	b.release(ctl)
	time.Sleep(40 * time.Millisecond) // a refresh tick would have fired by now, had anything been held
	c.Restore(t)
	if n := c.Redis.CommandCount(); n != 0 {
		t.Errorf("Redis saw %d commands from a budget-less deployment, want 0", n)
	}
}

func TestRedisLedger_NeverDeletesWhatItCannotDecode(t *testing.T) {
	slowRefresh(t, time.Hour)
	c := redisreplica.New(t, 1)
	b := replicaBudget(t, c.Replicas[0], 1.0, nil)
	key := redisx.InflightOrgKey.Key("org-1")
	future := fmt.Sprintf(`{"v":2,"kind":"ai.inflight","cost":9,"exp":%d,"extra":true}`, time.Now().Add(time.Hour).UnixMilli())
	live := fmt.Sprintf(`{"cost":0.25,"exp":%d}`, time.Now().Add(time.Hour).UnixMilli()) // v0: no envelope, still read
	c.Redis.HSet(key, "garbage", "not json", "future", future, "plain", live, "other-kind", `{"v":1,"kind":"something.else","cost":9,"exp":4102444800000}`)

	orgSum, _ := b.othersInFlight("u1", "org-1", nil)
	if orgSum < 0.25-1e-9 || orgSum > 0.25+1e-9 {
		t.Errorf("sum = %v, want only the readable record ($0.25)", orgSum)
	}
	for _, f := range []string{"garbage", "future", "plain", "other-kind"} {
		if c.Redis.HGet(key, f) == "" {
			t.Errorf("field %q was deleted by a reader", f)
		}
	}
}

func TestRedisLedger_WireShape(t *testing.T) {
	slowRefresh(t, time.Hour)
	c := redisreplica.New(t, 1)
	b := replicaBudget(t, c.Replicas[0], 1.0, nil)
	ctl := reservingStream("u1", "org-1", 0.5)
	if err := b.hold(ctl); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{redisx.InflightUserKey.Key("u1"), redisx.InflightOrgKey.Key("org-1")} {
		fields, _ := c.Redis.HKeys(key)
		if len(fields) != 1 {
			t.Fatalf("%s holds %v, want the one stream", key, fields)
		}
		v := c.Redis.HGet(key, fields[0])
		if !strings.HasPrefix(v, `{"v":1,"kind":"ai.inflight","cost":`) || !strings.Contains(v, `"exp":`) {
			t.Errorf("%s value = %s, want the enveloped {cost, exp}", key, v)
		}
		if strings.Contains(fields[0], "u1") {
			t.Errorf("the field %q names the user; it is a random stream handle", fields[0])
		}
	}
	// A personal stream has no org hash.
	solo := reservingStream("u7", "", 0.1)
	if err := b.hold(solo); err != nil {
		t.Fatal(err)
	}
	if keys := c.Redis.Keys(); len(keys) != 3 {
		t.Errorf("keys = %v, want user:u1, org:org-1 and user:u7", keys)
	}
}

func TestWithRedisLedger_NilClientKeepsTheInProcessLedger(t *testing.T) {
	b := NewAIBudget(nil, nil, WithRedisLedger(nil, nil))
	if _, ok := b.ledger.(*memLedger); !ok || b.shared {
		t.Errorf("ledger = %T shared=%v, want the in-process ledger", b.ledger, b.shared)
	}
	c := redisreplica.New(t, 1)
	b = NewAIBudget(nil, nil, WithRedisLedger(c.Replicas[0].Client, c.Replicas[0].Health))
	if _, ok := b.ledger.(*redisLedger); !ok || !b.shared {
		t.Errorf("ledger = %T shared=%v, want the Redis ledger", b.ledger, b.shared)
	}
}
