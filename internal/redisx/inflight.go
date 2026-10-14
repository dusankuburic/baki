package redisx

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/metrics"
)

// The AI budget's in-flight ledger over Redis (SURVEY_REALTIME 8.4, ADR 0028's spendLedger seam). A running chat
// stream's spend is not a usage row yet; every replica that checks the daily budget must see the streams the
// others run, or N replicas admit N times the budget in the gap. One hash per user and one per organisation, a
// field per running stream holding {cost, exp}.
//
// The ledger fails CLOSED: it is only consulted when a daily budget is configured, and a ledger that cannot answer
// makes the budget check refuse (service.ErrAIBudgetUnavailable). Guessing "nothing in flight" while Redis is down
// would let every replica spend freely.
//
// The overshoot bound the design promises: each stream publishes spend-so-far plus one more maximum response (the
// caller's reservation), written in the same pipeline as the read of the others' (Others), so of any two checks
// that overlap in time at least one sees the other's reservation. The reservation covers the 1 s refresh lag of a
// growing stream, so two replicas jointly stay within the budget plus the spend of one provider call that was
// already running when its stream was refused, i.e. what a single replica's in-process ledger allows.

var (
	// InflightUserKey and InflightOrgKey are the two hashes. The TTL is the backstop for a hash nobody refreshes
	// any more (a crashed replica's last stream): the per-field expiry (inflightLease) is what stops the orphan
	// counting, long before the key goes. 10 minutes is the longest a stream lives (maxChatStreamDuration) plus
	// slack.
	InflightUserKey = Declare(Spec{
		Name:    "ai.inflight-user",
		Kind:    KindHash,
		Pattern: "baki:ai:inflight:user:{scope}",
		TTL:     inflightKeyTTL,
		Version: 1,
		OnDown:  OnDownClosed,
		Owner:   "internal/service",
	})
	InflightOrgKey = Declare(Spec{
		Name:    "ai.inflight-org",
		Kind:    KindHash,
		Pattern: "baki:ai:inflight:org:{org}",
		TTL:     inflightKeyTTL,
		Version: 1,
		OnDown:  OnDownClosed,
		Owner:   "internal/service",
	})
)

const (
	inflightKeyTTL = 11 * time.Minute
	// inflightKind is the payload kind of a field's value.
	inflightKind = "ai.inflight"
	// inflightLease is how long a field counts after its last write. The owner rewrites it every second while the
	// stream runs, so a replica that dies stops counting after this long, with no one deleting anything.
	inflightLease = 30 * time.Second
	// inflightOpTimeout bounds one ledger round trip. A stream start waits for it (outside the stream-cap lock), so a slow
	// Redis delays that start by at most this long; a degraded Redis is not asked at all.
	inflightOpTimeout = 2 * time.Second
)

var (
	ledgerErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_ai_ledger_errors_total",
		Help: "AI budget in-flight ledger operations on Redis that failed, by op (hold, refresh, read, release, lost). A failed hold or read refuses the AI call with AI_BUDGET_UNAVAILABLE while a daily budget is set (the ledger fails closed); refresh and release failures are best effort and age out. lost: a held stream's entry had to be created again (the server evicted it for memory, or a peer pruned it while its owner stalled), so the next budget check on the replica refused once; a steady rate means Redis is out of memory.",
	}, []string{"op"})
	ledgerHeld = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_ai_ledger_held",
		Help: "Chat streams this replica currently holds in the shared AI budget in-flight ledger.",
	})
	ledgerExpired = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_ai_ledger_expired_total",
		Help: "In-flight ledger entries of other streams found past their lease and ignored: orphans of a replica that stopped without releasing them. A steady rate means replicas crash mid-stream.",
	})
)

func init() { metrics.MustRegister(ledgerErrors, ledgerHeld, ledgerExpired) }

// LedgerOp names the operation of a failed ledger call, the label of pad_ai_ledger_errors_total.
type LedgerOp string

const (
	LedgerHold    LedgerOp = "hold"
	LedgerRefresh LedgerOp = "refresh"
	LedgerRead    LedgerOp = "read"
	LedgerRelease LedgerOp = "release"
	// LedgerLost: a held stream's entry was found missing when it was written again (evicted for memory, or pruned as
	// expired during a stall); the next budget check fails closed.
	LedgerLost LedgerOp = "lost"
)

// ErrEntryLost: the ledger lost a record it held, so what it sums can no longer be trusted.
var ErrEntryLost = errors.New("redisx: the in-flight ledger lost an entry it held (evicted for memory?)")

// CountLedgerError counts one failed ledger operation.
func CountLedgerError(op LedgerOp) { ledgerErrors.WithLabelValues(string(op)).Inc() }

// SetLedgerHeld reports how many streams this replica holds.
func SetLedgerHeld(n int) { ledgerHeld.Set(float64(n)) }

// InflightEntry is one running stream's line in the ledger: Field identifies it (unique per stream, chosen by the
// owner), User and Org are who the spend is charged to (Org "" = personal), Cost is the stream's spend so far plus
// its reservation.
type InflightEntry struct {
	Field string
	User  string
	Org   string
	Cost  float64
	// Established: the entry was written before (it is a refresh or a check of a held stream, not its hold), so a
	// write that has to create the field again found it lost.
	Established bool
}

// inflightRecord is a field's value: {"v":1,"kind":"ai.inflight","cost":0.4,"exp":1760000000000}. exp is Unix ms.
type inflightRecord struct {
	Envelope
	Cost float64 `json:"cost"`
	Exp  int64   `json:"exp"`
}

// InflightLedger reads and writes the ledger on one client.
type InflightLedger struct {
	client *redis.Client
	now    func() time.Time
}

// NewInflightLedger builds the ledger on client (never nil: the caller falls back to its in-process ledger).
func NewInflightLedger(client *redis.Client) *InflightLedger {
	return &InflightLedger{client: client, now: time.Now}
}

// WithClock replaces the clock the leases are written and read with (tests).
func (l *InflightLedger) WithClock(now func() time.Time) *InflightLedger {
	l.now = now
	return l
}

func (l *InflightLedger) value(cost float64) (string, error) {
	b, err := json.Marshal(inflightRecord{
		Envelope: Envelope{V: 1, Kind: inflightKind},
		Cost:     cost,
		Exp:      l.now().Add(inflightLease).UnixMilli(),
	})
	return string(b), err
}

// queuePut adds the writes of e to pipe: its field in the user hash and, for an org stream, the org hash. The
// returned commands are the HSETs, whose reply says whether the field was new.
func (l *InflightLedger) queuePut(ctx context.Context, pipe redis.Pipeliner, e InflightEntry) ([]*redis.IntCmd, error) {
	v, err := l.value(e.Cost)
	if err != nil {
		return nil, err
	}
	uk := InflightUserKey.Key(e.User)
	sets := []*redis.IntCmd{pipe.HSet(ctx, uk, e.Field, v)}
	pipe.Expire(ctx, uk, inflightKeyTTL)
	if e.Org != "" {
		ok := InflightOrgKey.Key(e.Org)
		sets = append(sets, pipe.HSet(ctx, ok, e.Field, v))
		pipe.Expire(ctx, ok, inflightKeyTTL)
	}
	return sets, nil
}

// Put writes (or refreshes) the entries in one transaction. The first call for an entry is its hold; the one-second
// refresh of a growing stream is the same write.
func (l *InflightLedger) Put(ctx context.Context, entries ...InflightEntry) error {
	_, err := l.PutCreated(ctx, entries...)
	return err
}

// PutCreated is Put that also reports which entries (by Field) the server had no field for: their write created it.
// For an entry that was written before this is the sign of a lost record: the server evicted it for memory, or
// another replica pruned it as expired while its owner was stalled.
func (l *InflightLedger) PutCreated(ctx context.Context, entries ...InflightEntry) (created map[string]bool, err error) {
	if len(entries) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, inflightOpTimeout)
	defer cancel()
	pipe := l.client.TxPipeline() // the field and its key's expiry together: a cut between them must not leave a key nobody can evict
	sets := make([][]*redis.IntCmd, len(entries))
	for i, e := range entries {
		if sets[i], err = l.queuePut(ctx, pipe, e); err != nil {
			return nil, err
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for i, e := range entries {
		for _, c := range sets[i] {
			if c.Val() == 1 {
				if created == nil {
					created = map[string]bool{}
				}
				created[e.Field] = true
			}
		}
	}
	return created, nil
}

// Remove takes a stream out of both hashes.
func (l *InflightLedger) Remove(ctx context.Context, e InflightEntry) error {
	ctx, cancel := context.WithTimeout(ctx, inflightOpTimeout)
	defer cancel()
	pipe := l.client.Pipeline()
	pipe.HDel(ctx, InflightUserKey.Key(e.User), e.Field)
	if e.Org != "" {
		pipe.HDel(ctx, InflightOrgKey.Key(e.Org), e.Field)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// Others sums what the other streams have in flight: orgSum for org (when set), userSum for user. self is the
// calling stream's own field, skipped. When publish is non-nil its entry is written FIRST in the same pipeline, so
// a check publishes its reservation and reads the others' in one round trip, ordered: of two overlapping checks at
// least one sees the other (the overshoot bound above).
//
// Fields past their lease and fields that do not decode are skipped; an expired one is also deleted (it is ours to
// prune: its owner is gone), an undecodable one never is (another release may have written it).
func (l *InflightLedger) Others(ctx context.Context, user, org, self string, publish *InflightEntry) (orgSum, userSum float64, err error) {
	ctx, cancel := context.WithTimeout(ctx, inflightOpTimeout)
	defer cancel()
	pipe := l.client.TxPipeline()
	var published []*redis.IntCmd
	if publish != nil {
		var err error
		if published, err = l.queuePut(ctx, pipe, *publish); err != nil {
			return 0, 0, err
		}
	}
	uk := InflightUserKey.Key(user)
	users := pipe.HGetAll(ctx, uk)
	var orgs *redis.MapStringStringCmd
	ok := ""
	if org != "" {
		ok = InflightOrgKey.Key(org)
		orgs = pipe.HGetAll(ctx, ok)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, 0, err
	}
	// The caller's own entry was written before: if the write had to create it, the record was lost (evicted for
	// memory, or pruned while its owner stalled), and so may be the others' that this read is about to sum. The sum
	// cannot be trusted, and the budget fails closed on it, as it does when the ledger cannot be read.
	for _, c := range published {
		if publish.Established && c.Val() == 1 {
			return 0, 0, ErrEntryLost
		}
	}
	userSum, userStale := l.sum(users.Val(), self)
	var orgStale []string
	if orgs != nil {
		orgSum, orgStale = l.sum(orgs.Val(), self)
	}
	l.prune(ctx, uk, userStale)
	l.prune(ctx, ok, orgStale)
	return orgSum, userSum, nil
}

// sum totals the live costs of a hash except self and returns the expired fields it saw.
func (l *InflightLedger) sum(fields map[string]string, self string) (total float64, stale []string) {
	nowMs := l.now().UnixMilli()
	for field, raw := range fields {
		if field == self {
			continue
		}
		var rec inflightRecord
		if Decode(inflightKind, 1, []byte(raw), &rec) != ResultOK {
			continue
		}
		if rec.Exp <= nowMs {
			stale = append(stale, field)
			continue
		}
		if rec.Cost <= 0 || math.IsNaN(rec.Cost) || math.IsInf(rec.Cost, 0) {
			continue
		}
		total += rec.Cost
	}
	return total, stale
}

// prune deletes expired fields, best effort and off the answer's path.
func (l *InflightLedger) prune(ctx context.Context, key string, fields []string) {
	if len(fields) == 0 || key == "" {
		return
	}
	ledgerExpired.Add(float64(len(fields)))
	// A failed prune changes nothing: the fields stay past their lease and are skipped again.
	_ = l.client.HDel(ctx, key, fields...).Err()
}

// ErrorClass reduces a Redis error to a short class ("timeout", "connection refused", ...) for a log line: the
// error text can carry the server's address.
func ErrorClass(err error) string { return errorClass(err) }
