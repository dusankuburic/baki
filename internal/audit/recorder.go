// Package audit records the audit trail: who did what to which resource, from where, for which organisation.
// A Recorder builds each row from the context (the actor auth placed there, the request's reqinfo.Info) and
// writes it through a bounded worker pool that batches inserts, retries once, spills overflow to disk and
// diverts what it still cannot write to a redacted log line. It does not depend on HTTP, so services and
// background jobs record the same way handlers do (ADR 0008).
package audit

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/reqinfo"
	storageif "pad-analyzer/internal/storage/interfaces"
)

const (
	queueSize = 256
	workers   = 8
	writeTTL  = 5 * time.Second

	// Batching knobs: a worker flushes on the first of (flushSize reached | flushDelay elapsed | shutdown) —
	// one multi-row INSERT per batch, so the 256-buffer rarely fills and drops.
	flushSize    = 64
	flushDelay   = 100 * time.Millisecond
	writeRetries = 1 // bounded retry on a transient flush failure
	retryDelay   = 200 * time.Millisecond

	// defaultShutdownBudget bounds how long Stop waits for the workers to flush: an unbounded wait once blew
	// past fx's StopTimeout and the process was SIGKILLed mid-shutdown. Events still queued at expiry are
	// dropped with a warning — the lesser harm.
	defaultShutdownBudget = 5 * time.Second
)

// Entry is one audited action; the recorder adds who acted, from where and when.
type Entry struct {
	Action       string
	ResourceType string
	ResourceID   string
	// OrgID is the organisation the action belongs to, "" when none.
	OrgID string
	// Meta never carries a secret; values may be personal data (the log fallback keeps only the keys).
	Meta map[string]string
}

// Config tunes a Recorder.
type Config struct {
	// SpillDir is the directory of the on-disk overflow queue; "" disables it (overflow goes to the log).
	SpillDir string
	// SpillMaxBytes caps the queue file; <= 0 means 10 MB.
	SpillMaxBytes int64
	// Instance names this replica in its spill file (audit-spill-<Instance>.jsonl), so replicas sharing a directory
	// do not rewrite each other's file; "" is "unnamed".
	Instance string
	// AdoptAfter is how long another replica's spill file must have gone untouched before this one takes it over;
	// <= 0 means two minutes.
	AdoptAfter time.Duration
}

// Recorder writes audit rows. A Recorder without a store (desktop: no server-side store) records nothing, and so
// does a nil *Recorder.
type Recorder struct {
	store storageif.AuditStore
	spill *spillStore

	queue chan *storageif.AuditEvent
	// sink, set by NewWithSink (tests), names a channel that takes each row in place of the store; nil
	// channel: the row goes to the store.
	sink    func() chan<- *storageif.AuditEvent
	started atomic.Bool
	closed  atomic.Bool // guards against a send on the closed queue

	startOnce, stopOnce sync.Once
	workers             sync.WaitGroup
	reaperStop          chan struct{}
	reaper              sync.WaitGroup
	shutdownBudget      time.Duration

	// instance and adoptAfter come from Config; adopted holds the spill files of other replicas this one replays
	// (claimOrphans), guarded by adoptMu.
	instance   string
	adoptAfter time.Duration
	adoptMu    sync.Mutex
	adopted    []*spillStore
}

// New returns a recorder writing to store; Start starts its pool. Until then, and for a recorder never started,
// each event is a detached best-effort write of its own.
func New(store storageif.AuditStore, cfg Config) *Recorder {
	r := &Recorder{store: store, shutdownBudget: defaultShutdownBudget, instance: cfg.Instance, adoptAfter: cfg.AdoptAfter}
	if r.adoptAfter <= 0 {
		r.adoptAfter = defaultAdoptAfter
	}
	if cfg.SpillDir != "" {
		s, err := newSpillStore(cfg.SpillDir, cfg.Instance, cfg.SpillMaxBytes)
		if err != nil {
			slog.Warn("audit spill queue disabled (using log fallback only)", "error", err)
		} else {
			r.spill = s
		}
	}
	if store != nil {
		r.queue = make(chan *storageif.AuditEvent, queueSize)
	}
	return r
}

// NewCapture returns a running recorder without workers whose queue is ch, so every event it records lands on
// ch. For tests that assert on audit rows.
func NewCapture(ch chan *storageif.AuditEvent) *Recorder {
	r := &Recorder{queue: ch, shutdownBudget: defaultShutdownBudget}
	r.started.Store(true)
	return r
}

// NewWithSink returns a recorder over store that is never started and asks sink, on each row, where to put it:
// a non-nil channel takes the row (in order, without blocking: a full channel diverts it to the log fallback),
// else the row is a detached write to store. For tests that build handlers first and decide later whether to
// capture their rows. A nil store records nothing.
func NewWithSink(store storageif.AuditStore, sink func() chan<- *storageif.AuditEvent) *Recorder {
	r := New(store, Config{})
	r.sink = sink
	return r
}

// Start starts the worker pool and, with a spill queue, the reaper that drains it back into the pool whenever
// capacity returns (which also recovers events a previous boot left on disk). Idempotent.
func (r *Recorder) Start() {
	if r == nil || r.queue == nil {
		return
	}
	r.startOnce.Do(func() {
		r.workers.Add(workers)
		for range workers {
			go r.worker()
		}
		if r.spill != nil {
			r.adoptOrphans()
			r.reaperStop = make(chan struct{})
			r.reaper.Add(1)
			go r.reapSpill()
		}
		r.started.Store(true)
	})
}

// Stop closes the pool and waits, within the shutdown budget, for the workers to flush what is queued, then
// stops the reaper. Events recorded afterwards go to the log fallback. Idempotent.
func (r *Recorder) Stop() {
	if r == nil || !r.started.Load() {
		return
	}
	r.stopOnce.Do(func() {
		r.closed.Store(true)
		close(r.queue)
		if !waitWithBudget(&r.workers, r.shutdownBudget) {
			slog.Warn("audit: workers did not drain within the shutdown budget; remaining queued events are dropped", "budget", r.shutdownBudget)
			return
		}
		if r.reaperStop != nil {
			close(r.reaperStop)
			if !waitWithBudget(&r.reaper, r.shutdownBudget) {
				slog.Warn("audit: spill reaper did not stop within the shutdown budget", "budget", r.shutdownBudget)
			}
		}
	})
}

// Record writes one audit row for e. Who acted comes from ctx (auth.ActorFromContext: the actor set with
// auth.WithActor — a background job's auth.SystemActor — else the request's claims; the user and email from the
// claims alone), the client address and user agent from the request's reqinfo.Info. It never blocks on the database and never fails the caller.
func (r *Recorder) Record(ctx context.Context, e Entry) {
	if r == nil || r.queue == nil {
		return
	}
	r.submit(NewEvent(ctx, e))
}

type erasedActorKey struct{}

// WithErasedActor marks ctx's actor as erased: the account was deleted earlier in this request, so the rows the
// request records afterwards (the deletion itself, the credentials it disabled) keep the action and its resource but
// none of the actor's identity (user, email, address, user agent, token), as erasure left the actor's earlier rows.
func WithErasedActor(ctx context.Context) context.Context {
	return context.WithValue(ctx, erasedActorKey{}, true)
}

// NewEvent builds the row Record writes for e.
func NewEvent(ctx context.Context, e Entry) *storageif.AuditEvent {
	ev := &storageif.AuditEvent{
		ID:           uuid.NewString(),
		Action:       e.Action,
		ResourceType: e.ResourceType,
		ResourceID:   e.ResourceID,
		OrgID:        e.OrgID,
		Meta:         e.Meta,
		CreatedAt:    time.Now().UTC(),
	}
	if a := auth.ActorFromContext(ctx); a != nil {
		ev.ActorKind, ev.TokenID, ev.ActorJob = a.Kind.String(), a.TokenID, a.Job
	}
	// The user columns name an account, so they come from the authenticated claims: local mode's single user
	// (an actor without claims) is recorded by its kind alone, with the empty user its rows always had.
	if c := auth.ClaimsFromContext(ctx); c != nil {
		ev.UserID, ev.Email = c.UserID, c.Email
	}
	if info := reqinfo.From(ctx); info != nil {
		ev.IP, ev.UserAgent = info.ClientIP, info.UserAgent
	}
	if erased, _ := ctx.Value(erasedActorKey{}).(bool); erased {
		ev.UserID, ev.Email, ev.IP, ev.UserAgent, ev.TokenID = "", "", "", "", ""
	}
	return ev
}

// submit hands ev to the running pool; before Start (or for a recorder never started) it is a detached
// best-effort write to the store. A NewWithSink recorder's sink, when it names a channel, takes ev instead.
func (r *Recorder) submit(ev *storageif.AuditEvent) {
	if r.sink != nil {
		if ch := r.sink(); ch != nil {
			select {
			case ch <- ev:
			default:
				r.divert("full", ev)
			}
			return
		}
	}
	if !r.started.Load() {
		saveDetached(r.store, ev)
		return
	}
	if _, reason := r.enqueue(ev); reason != "" {
		r.divert(reason, ev)
	}
}

// divert places an event the pool did not take. On "full" it spills to the durable on-disk queue first so a
// transient overload can be replayed, and goes to the log fallback only with no spill store or at its cap.
// "closed" (shutting down) always goes to the fallback: the reaper is stopping too.
func (r *Recorder) divert(reason string, ev *storageif.AuditEvent) {
	if reason == "full" && r.spill != nil && r.spill.Spill(ev) {
		return
	}
	fallback(reason, ev)
}

// saveDetached writes ev on its own, off the caller's path and context.
func saveDetached(store storageif.AuditStore, ev *storageif.AuditEvent) {
	if store == nil {
		return
	}
	// #nosec G118 -- detached best-effort audit write; must not be tied to request ctx.
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Warn("audit fallback goroutine panicked", "action", ev.Action, "err", rec)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), writeTTL)
		defer cancel()
		if err := store.SaveAuditEvent(ctx, ev); err != nil {
			slog.Warn("audit log write failed", "action", ev.Action, "err", err)
		}
	}()
}

// enqueue attempts to deliver ev to the pool: (true, "") enqueued, (false, "closed") when shutting down,
// (false, "full") when the bounded queue is full. The atomic flag is a fast path; the recover closes the
// residual TOCTOU window of a send racing Stop's close.
func (r *Recorder) enqueue(ev *storageif.AuditEvent) (sent bool, reason string) {
	defer func() {
		if rec := recover(); rec != nil {
			sent, reason = false, "closed"
		}
	}()
	if r.closed.Load() {
		return false, "closed"
	}
	select {
	case r.queue <- ev:
		return true, ""
	default:
		return false, "full"
	}
}

// waitWithBudget waits for wg for at most d. Returns false on timeout; the helper goroutine it leaves behind
// exits when wg eventually completes.
func waitWithBudget(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// batchSink persists a batch of audit events in one round trip (storageif.AuditStore's SaveAuditEvents).
type batchSink interface {
	SaveAuditEvents(ctx context.Context, events []*storageif.AuditEvent) error
}

// writer is the narrow write capability the workers need, so tests can supply a minimal fake.
type writer interface {
	SaveAuditEvent(ctx context.Context, event *storageif.AuditEvent) error
}

func (r *Recorder) worker() {
	defer r.workers.Done()
	var buf []*storageif.AuditEvent

	// On an unrecoverable panic outside the flush path, divert buffered-but-unflushed events to the fallback
	// sink rather than dropping them (events mid-write are salvaged by the flush-local recover below).
	defer func() {
		if rec := recover(); rec != nil {
			slog.Warn("audit worker panicked, salvaging buffered events", "err", rec, "buffered", len(buf))
			salvageBatch(r.store, buf)
		}
	}()

	var timer *time.Timer
	var timerC <-chan time.Time

	flush := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			timerC = nil
		}
		if len(buf) == 0 {
			return
		}
		batch := buf
		buf = nil
		// A panic inside the write path would otherwise lose the whole in-flight batch and kill the worker:
		// isolate the batch per-event and let the worker keep running.
		defer func() {
			if rec := recover(); rec != nil {
				slog.Warn("audit flush panicked, isolating in-flight batch", "err", rec, "events", len(batch))
				salvageBatch(r.store, batch)
			}
		}()
		writeBatch(r.store, r.store, batch)
	}
	for {
		select {
		case ev, ok := <-r.queue:
			if !ok {
				flush() // drain the accumulated batch on shutdown
				return
			}
			buf = append(buf, ev)
			if len(buf) >= flushSize {
				flush()
			} else if timer == nil {
				timer = time.NewTimer(flushDelay)
				timerC = timer.C
			}
		case <-timerC:
			flush()
		}
	}
}

// adoptEvery is how often the reaper looks for spill files of replicas that are gone: a replica that dies while the
// others run leaves a file nobody would otherwise see before a restart.
const adoptEvery = time.Minute

// reapSpill drains the on-disk spill queue back into the pool whenever there is capacity, on a 500 ms tick
// (sub-second replay latency without contending with the hot write path), and looks for abandoned spill files of
// other replicas once a minute.
func (r *Recorder) reapSpill() {
	defer r.reaper.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	adopt := time.NewTicker(adoptEvery)
	defer adopt.Stop()
	for {
		select {
		case <-r.reaperStop:
			return
		case <-adopt.C:
			r.adoptOrphans()
		case <-ticker.C:
			r.replaySpilled()
		}
	}
}

// adoptOrphans takes over the spill files of replicas that are gone, and the claims a previous run of this instance
// left half replayed (claimOrphans decides what is abandoned). The files are replayed by the same reaper as our own.
func (r *Recorder) adoptOrphans() {
	if r.spill == nil {
		return
	}
	dir := filepath.Dir(r.spill.path)
	paths := append(ownClaims(dir, r.instance), claimOrphans(dir, r.instance, r.adoptAfter)...)
	r.adoptMu.Lock()
	defer r.adoptMu.Unlock()
	for _, p := range paths {
		if slices.ContainsFunc(r.adopted, func(s *spillStore) bool { return s.path == p }) {
			continue
		}
		s, err := openAdopted(p, r.spill.gauge, r.spill.maxBytes)
		if err != nil {
			slog.Warn("audit: cannot read an adopted spill file; leaving it for the next scan", "file", filepath.Base(p), "error", err)
			continue
		}
		r.adopted = append(r.adopted, s)
		slog.Info("audit: replaying the spill file of another replica", "file", filepath.Base(p), "events", s.depth)
	}
}

// spillStores is our own spill file and the claims we replay.
func (r *Recorder) spillStores() []*spillStore {
	r.adoptMu.Lock()
	defer r.adoptMu.Unlock()
	return append([]*spillStore{r.spill}, r.adopted...)
}

// replaySpilled moves drained spill events into the pool, only while it has ample room (< half full) so replay
// never starves fresh events. A partial-batch failure re-spills the unconsumed remainder (without
// double-counting the metric) and yields. A claim that is drained is deleted.
func (r *Recorder) replaySpilled() {
	if r.spill == nil {
		return
	}
	for _, s := range r.spillStores() {
		if !r.replayFrom(s) {
			return
		}
		if s != r.spill && s.removeIfDrained() {
			r.dropAdopted(s)
		}
	}
}

func (r *Recorder) dropAdopted(s *spillStore) {
	r.adoptMu.Lock()
	defer r.adoptMu.Unlock()
	r.adopted = slices.DeleteFunc(r.adopted, func(o *spillStore) bool { return o == s })
}

// replayFrom drains s into the pool; false: the pool has no more room, stop for this tick.
func (r *Recorder) replayFrom(s *spillStore) bool {
	for {
		if len(r.queue) >= queueSize/2 {
			return false // leave headroom for fresh events
		}
		batch := s.Drain(flushSize)
		if len(batch) == 0 {
			return true
		}
		var failed []*storageif.AuditEvent
		for _, ev := range batch {
			if sent, _ := r.enqueue(ev); !sent {
				failed = append(failed, ev)
			} else {
				recordAuditSpillReplayed()
			}
		}
		if len(failed) > 0 {
			// Pool filled/closed mid-batch — put the unconsumed remainder back (reSpill doesn't bump the
			// spilled metric) and yield this tick.
			s.reSpill(failed)
			return false
		}
	}
}

// writeBatch persists a batch with a bounded retry: a single batched write, falling back to per-event writes so
// a bad row can't poison the batch. Events still failing after retry go to the log fallback sink
// (pad_audit_dropped_total{reason="write_failed"}).
func writeBatch(w writer, sink batchSink, events []*storageif.AuditEvent) {
	if err := saveBatchWithRetry(sink, events); err == nil {
		return
	}
	// Batch failed (likely one bad row) — isolate via per-event writes below.
	for _, ev := range events {
		writeOne(w, ev)
	}
}

// saveBatchWithRetry attempts the batched INSERT up to writeRetries+1 times with a short backoff between
// attempts.
func saveBatchWithRetry(sink batchSink, events []*storageif.AuditEvent) error {
	var lastErr error
	for attempt := 0; attempt <= writeRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), writeTTL)
		err := sink.SaveAuditEvents(ctx, events)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt < writeRetries {
			time.Sleep(retryDelay)
		}
	}
	return lastErr
}

// writeOne saves a single event with one retry, diverting to the log fallback sink (and bumping the drop
// metric) on persistent failure.
func writeOne(w writer, ev *storageif.AuditEvent) {
	save := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), writeTTL)
		defer cancel()
		return w.SaveAuditEvent(ctx, ev)
	}
	if err := save(); err != nil {
		time.Sleep(retryDelay)
		if err := save(); err != nil {
			slog.Warn("audit log write failed after retry", "action", ev.Action, "err", err)
			fallback("write_failed", ev)
		}
	}
}

// salvageBatch persists events that were in flight when a worker hit a panic: each written independently with
// its own panic guard (a re-panic can't abort the salvage), failures to the fallback sink.
func salvageBatch(w writer, events []*storageif.AuditEvent) {
	for _, ev := range events {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					fallback("salvage_failed", ev)
				}
			}()
			writeOne(w, ev)
		}()
	}
}
