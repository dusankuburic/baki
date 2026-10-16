package redisx

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ReplicaLiveKey says a replica is running: written by the replica every LivenessEvery with a TTL of LivenessTTL, and
// deleted when it stops cleanly. Its absence is the one positive sign that a replica is gone (a replica that does not
// answer a command may only be slow, restarting its subscription, or cut off from Redis): the stream commands use it
// to answer a stop for a dead replica's stream as an ended stream instead of 503 until the stream's snapshot expires
// (ADR 0033, addendum (close-out)). Written only by a deployment that can run more than one replica.
//
// The key can also go while its replica lives: Redis restarted from a snapshot older than the TTL, the replica cut off
// from Redis, an eviction. So that its absence still means "the replica's streams are gone", the writer stops what it
// published once it has gone without a successful write for long enough that the key may lapse (OnStale), writes the
// key again the moment it can (Kick), and the readers trust a missing key only when their own view of Redis has been
// steady for a whole TTL (internal/service).
var ReplicaLiveKey = Declare(Spec{
	Name:    "replica.live",
	Kind:    KindString,
	Pattern: "baki:replica:live:{origin}",
	TTL:     LivenessTTL,
	Version: 1,
	OnDown:  OnDownOpen,
	Owner:   "internal/redisx",
})

const (
	// LivenessTTL bounds how long a dead replica still looks alive: three refreshes, so one lost write does not make a
	// live replica look dead.
	LivenessTTL = 15 * time.Second
	// livenessOpTimeout bounds one write or read of the key.
	livenessOpTimeout = time.Second
	// livenessRetry is how soon a failed write is tried again.
	livenessRetry = 500 * time.Millisecond
	livenessKind  = "replica.live"
)

// LivenessEvery is how often a replica rewrites its key. A var for tests.
var LivenessEvery = 5 * time.Second

// livenessRecord is the key's value: {"v":1,"kind":"replica.live","at":<unix ms>}. Readers only ask whether the key
// exists; the time is for an operator reading Redis.
type livenessRecord struct {
	Envelope
	At int64 `json:"at"`
}

// Liveness keeps one replica's ReplicaLiveKey written while it runs. A nil *Liveness does nothing.
type Liveness struct {
	client *redis.Client
	origin string
	every  time.Duration
	// staleAfter is how long the writer may go without a successful write before OnStale runs: two missed refreshes,
	// and never later than one refresh before the last good write's key lapses. 0: never (a refresh interval that does
	// not fit in the TTL, tests only).
	staleAfter time.Duration
	kick       chan struct{}
	// lastOK is when the last successful write was sent (unix nanoseconds): the key lapses LivenessTTL after the
	// server took it, which is no earlier than that.
	lastOK atomic.Int64

	// listening and onStale are set before Start (WhileListening, OnStale).
	listening func() bool
	onStale   func()

	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

// NewLiveness is the liveness writer of the replica named origin (BootOrigin). nil for a nil client or an empty origin.
func NewLiveness(client *redis.Client, origin string) *Liveness {
	if client == nil || origin == "" {
		return nil
	}
	return &Liveness{client: client, origin: origin, every: LivenessEvery, staleAfter: max(min(LivenessTTL-LivenessEvery, 2*LivenessEvery), 0), kick: make(chan struct{}, 1)}
}

// WhileListening makes the writes depend on listening (the replica's command subscription): the key then says "a
// command sent now is received", and a replica whose subscription cannot come back lets it lapse like a dead one,
// stopping its streams first (OnStale). Call it before Start.
func (l *Liveness) WhileListening(listening func() bool) {
	if l != nil {
		l.listening = listening
	}
}

// OnStale has fn run at every refresh while the writer has gone without a successful write for long enough that the
// key may lapse before the next one: the replica stops the work that others would otherwise take for gone with it.
// Call it before Start.
func (l *Liveness) OnStale(fn func()) {
	if l != nil {
		l.onStale = fn
	}
}

// Kick writes the key now rather than at the next refresh: Redis, or the replica's command subscription, is back. It
// never blocks; a kick before Start is served when the writer starts.
func (l *Liveness) Kick() {
	if l == nil {
		return
	}
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Start writes the key now and then every LivenessEvery (and at each Kick) until Stop. A failed write is retried at the
// next tick: the key may lapse during a Redis outage, which makes the replica look gone only to a command that also got
// no answer from it, and before it can, OnStale has run. Starting a started writer does nothing.
func (l *Liveness) Start() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stop != nil {
		return
	}
	l.stop, l.done = make(chan struct{}), make(chan struct{})
	l.lastOK.Store(time.Now().UnixNano())
	l.write()
	go l.run(l.stop, l.done)
}

func (l *Liveness) run(stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(l.every)
	defer t.Stop()
	var retry <-chan time.Time
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		case <-l.kick:
		case <-retry:
		}
		retry = nil
		// Before the write as well as after a failed one: a replica that stalled past the TTL must not carry on as if
		// its key had held, even though the write it makes on waking succeeds.
		l.checkStale()
		if !l.write() {
			l.checkStale()
			// A write Redis refused while the replica listens (the first command on a connection the outage broke) is
			// tried again shortly, not a whole refresh later.
			if l.listening == nil || l.listening() {
				retry = time.After(min(livenessRetry, l.every))
			}
		}
	}
}

// checkStale runs OnStale when no write has succeeded for staleAfter.
func (l *Liveness) checkStale() {
	if l.onStale == nil || l.staleAfter <= 0 || time.Since(time.Unix(0, l.lastOK.Load())) < l.staleAfter {
		return
	}
	LimitedWarn("replica_liveness_stale", "redis: this replica could not keep its liveness key; it stops the chat streams the other replicas know of, which would otherwise take them for gone while they run", nil)
	l.onStale()
}

// write writes the key and reports whether Redis took it. Nothing is written while the replica is not listening.
func (l *Liveness) write() bool {
	if l.listening != nil && !l.listening() {
		return false
	}
	b, err := json.Marshal(livenessRecord{Envelope: Envelope{V: 1, Kind: livenessKind}, At: time.Now().UnixMilli()})
	if err != nil {
		return false
	}
	sent := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), livenessOpTimeout)
	defer cancel()
	if err := l.client.Set(ctx, ReplicaLiveKey.Key(l.origin), b, LivenessTTL).Err(); err != nil {
		LimitedWarn("replica_liveness", "redis: this replica's liveness key could not be written; the other replicas may take it for gone", err)
		return false
	}
	l.lastOK.Store(sent.UnixNano())
	return true
}

// Stop ends the writes and deletes the key (best effort: otherwise it lapses after LivenessTTL), so a replica that
// stopped cleanly is known to be gone at once.
func (l *Liveness) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	stop, done := l.stop, l.done
	l.stop, l.done = nil, nil
	l.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
	ctx, cancel := context.WithTimeout(context.Background(), livenessOpTimeout)
	defer cancel()
	_ = l.client.Del(ctx, ReplicaLiveKey.Key(l.origin)).Err()
}

// ReplicaAlive reports whether the replica named origin has its liveness key; err is set when Redis did not answer,
// which says nothing about the replica.
func ReplicaAlive(ctx context.Context, client *redis.Client, origin string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), livenessOpTimeout)
	defer cancel()
	n, err := client.Exists(ctx, ReplicaLiveKey.Key(origin)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
