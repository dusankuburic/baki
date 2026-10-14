package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"pad-core/logger"

	"pad-analyzer/internal/redisx"
)

// ledgerRefreshEvery is how often a held stream's cost is rewritten: the cost grows while the stream runs, and
// the entry's lease (redisx) only holds while its owner keeps writing. The same cadence as the resume mirror.
var ledgerRefreshEvery = time.Second

// redisLedger is the spendLedger shared across replicas: the streams every replica runs, so a replica's budget
// check counts what the others have in flight. The wire format, keys and lease are redisx.InflightLedger's.
// ErrAIBudgetUnavailable follows from any error here — the AIBudget wraps it — so with Redis down and a daily
// budget set, new turns, loop re-checks and the wrapper's per-call hook all refuse, and a running stream stops at
// its next re-check (ADR 0028 addendum in docs/architecture/ai-providers.md). With Redis marked degraded (health)
// the refusal is immediate and sends nothing: the answer is the same, and a round trip per call is what makes an
// outage look like a hung server.
type redisLedger struct {
	led    *redisx.InflightLedger
	health *redisx.Health
	every  time.Duration // the refresh interval, read once at construction

	// lost is set when a refresh had to create a held stream's record again (the server evicted it for memory, or a
	// peer pruned it while this replica stalled); the next others() reports it once and clears it.
	lost atomic.Bool

	mu      sync.Mutex
	held    map[inFlightSpend]string // stream → its field in the hashes
	running bool                     // the refresh goroutine is alive; it ends when held empties
}

// errLedgerDegraded: Redis is marked degraded, so the ledger was not asked.
var errLedgerDegraded = errors.New("ai budget: the shared ledger's Redis is degraded")

func newRedisLedger(client *redis.Client, health *redisx.Health) *redisLedger {
	return &redisLedger{led: redisx.NewInflightLedger(client), health: health, every: ledgerRefreshEvery, held: map[inFlightSpend]string{}}
}

func (l *redisLedger) entry(e inFlightSpend, field string) redisx.InflightEntry {
	user, org := e.spender()
	return redisx.InflightEntry{Field: field, User: user, Org: org, Cost: e.inflight()}
}

// newLedgerField names one stream in the hashes. It is random, not the stream ID: a stream ID is chosen by the
// client and unique only within a replica, and two replicas' streams must never share a field.
func newLedgerField() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (l *redisLedger) hold(e inFlightSpend) error {
	if l.health.Degraded() {
		redisx.CountLedgerError(redisx.LedgerHold)
		return errLedgerDegraded
	}
	field := newLedgerField()
	if err := l.led.Put(context.Background(), l.entry(e, field)); err != nil {
		redisx.CountLedgerError(redisx.LedgerHold)
		return err
	}
	l.mu.Lock()
	l.held[e] = field
	redisx.SetLedgerHeld(len(l.held))
	if !l.running {
		l.running = true
		go l.refreshLoop()
	}
	l.mu.Unlock()
	return nil
}

func (l *redisLedger) release(e inFlightSpend) error {
	l.mu.Lock()
	field, ok := l.held[e]
	delete(l.held, e)
	redisx.SetLedgerHeld(len(l.held))
	l.mu.Unlock()
	if !ok {
		return nil // never held here: refused before admission, or no budget was set when it started
	}
	if l.health.Degraded() {
		redisx.CountLedgerError(redisx.LedgerRelease)
		return errLedgerDegraded // nothing is sent while Redis is away; the entry ages out with its lease
	}
	if err := l.led.Remove(context.Background(), l.entry(e, field)); err != nil {
		redisx.CountLedgerError(redisx.LedgerRelease)
		return err // the entry ages out with its lease
	}
	return nil
}

func (l *redisLedger) others(ctx context.Context, scope, orgID string, self inFlightSpend) (orgSum, userSum float64, err error) {
	if l.health.Degraded() {
		redisx.CountLedgerError(redisx.LedgerRead)
		return 0, 0, errLedgerDegraded
	}
	if l.lost.Swap(false) {
		redisx.CountLedgerError(redisx.LedgerLost)
		return 0, 0, redisx.ErrEntryLost
	}
	var publish *redisx.InflightEntry
	selfField := ""
	if self != nil {
		l.mu.Lock()
		field, ok := l.held[self]
		l.mu.Unlock()
		if ok {
			selfField = field
			// Published first, in the read's own round trip: of two checks that overlap, one sees the other.
			pe := l.entry(self, field)
			pe.Established = true // it was held (written) before: finding it gone means it was lost
			publish = &pe
		}
	}
	orgSum, userSum, err = l.led.Others(ctx, scope, orgID, selfField, publish)
	switch {
	case errors.Is(err, redisx.ErrEntryLost):
		redisx.CountLedgerError(redisx.LedgerLost)
	case err != nil:
		redisx.CountLedgerError(redisx.LedgerRead)
	}
	return orgSum, userSum, err
}

// refreshLoop rewrites every held stream's cost each tick and ends when none is held (the next hold starts it
// again), so an idle replica runs nothing. A failed tick is counted and logged; the next tick tries again, and
// the checks fail on their own account while Redis is down.
func (l *redisLedger) refreshLoop() {
	t := time.NewTicker(l.every)
	defer t.Stop()
	for range t.C {
		l.mu.Lock()
		if len(l.held) == 0 {
			l.running = false
			l.mu.Unlock()
			return
		}
		l.mu.Unlock()
		if l.health.Degraded() {
			continue // nothing is sent; the checks refuse on their own account
		}
		l.writeRefresh(l.entriesHeld())
	}
}

// entriesHeld is a copy of the held set as ledger entries. inflight() takes the stream's own lock: the costs are
// read outside the ledger's.
func (l *redisLedger) entriesHeld() []redisx.InflightEntry {
	l.mu.Lock()
	held := make(map[inFlightSpend]string, len(l.held))
	for e, f := range l.held {
		held[e] = f
	}
	l.mu.Unlock()
	entries := make([]redisx.InflightEntry, 0, len(held))
	for e, f := range held {
		entry := l.entry(e, f)
		entry.Established = true
		entries = append(entries, entry)
	}
	return entries
}

// writeRefresh writes entries and settles what the write raced with. A stream released while the write was in flight
// is removed again (the write would otherwise bring it back for a whole lease and count against the budget); one
// whose record the write had to create was lost, which the next check reports.
func (l *redisLedger) writeRefresh(entries []redisx.InflightEntry) {
	created, err := l.led.PutCreated(context.Background(), entries...)
	if err != nil {
		redisx.CountLedgerError(redisx.LedgerRefresh)
		logLedgerFailure("refresh", err)
		return
	}
	l.mu.Lock()
	still := make(map[string]bool, len(l.held))
	for _, f := range l.held {
		still[f] = true
	}
	l.mu.Unlock()
	for _, e := range entries {
		if !still[e.Field] {
			// Released meanwhile: take it out again and do not read its re-creation as a loss.
			if rerr := l.led.Remove(context.Background(), e); rerr != nil {
				redisx.CountLedgerError(redisx.LedgerRelease)
			}
			continue
		}
		if created[e.Field] {
			l.lost.Store(true)
		}
	}
}

// ledgerUnavailable is the budget check's refusal when the shared ledger cannot answer: the same code and class as
// an unreadable spend (AI_BUDGET_UNAVAILABLE, ErrAIBudgetUnavailable), with copy that says what is wrong. The
// cause is logged (rate limited: with Redis down every turn and every loop iteration lands here) and never shown.
func ledgerUnavailable(scope string, cause error) error {
	if !isContextEnd(cause) {
		logLedgerFailure("check", cause)
	}
	return &budgetError{
		kind:  ErrAIBudgetUnavailable,
		msg:   "couldn't verify today's AI budget: the shared usage counter is unreachable — try again in a moment",
		cause: cause,
	}
}

func isContextEnd(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// lastLedgerLog is the UnixNano of the last ledger failure logged.
var lastLedgerLog atomic.Int64

// logLedgerFailure warns at most every 10 s: the class and the operation, never the error text (it can carry the
// Redis address).
func logLedgerFailure(op string, err error) {
	now := time.Now().UnixNano()
	last := lastLedgerLog.Load()
	if now-last < int64(10*time.Second) || !lastLedgerLog.CompareAndSwap(last, now) {
		return
	}
	logger.Warn("ai budget: the shared in-flight ledger failed; budget checks refuse while it does", "op", op, "error", redisx.ErrorClass(err))
}
