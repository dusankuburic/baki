package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"pad-analyzer/internal/lifecycle"
)

// RevocationSource is the durable, replica-shared revocation store that a
// CachedBlacklist fronts (the Postgres table in cloud mode). Unlike
// BlacklistStore it exposes the lookup on the caller's context WITHOUT a
// fail-closed decision baked in — the cache decides, because it knows whether
// it holds a local copy to answer from.
type RevocationSource interface {
	Add(jti string, ttl time.Duration)
	AddIfAbsent(jti string, ttl time.Duration) (added bool, err error)
	// IsRevokedCtx reports whether jti is revoked and unexpired; a store error
	// is returned as such.
	IsRevokedCtx(ctx context.Context, jti string) (bool, error)
	// LoadActive returns every unexpired entry with its expiry.
	LoadActive(ctx context.Context) (map[string]time.Time, error)
	Stop()
}

// RevocationPublisher fans one revocation out to the peer replicas.
type RevocationPublisher interface {
	Publish(ctx context.Context, channel string, payload []byte) error
}

// RevocationSubscriber delivers the peers' revocations. Subscribe BLOCKS: it
// opens a subscription on channel, calls onReady once established, then calls
// onMessage for every payload until ctx is done (returns nil) or the
// subscription breaks (returns the error — the cache treats the gap as lost
// messages, resubscribes and reloads).
type RevocationSubscriber interface {
	Subscribe(ctx context.Context, channel string, onReady func(), onMessage func(payload []byte)) error
}

// RevocationChannel is the pub/sub channel revocations are fanned out on.
const RevocationChannel = "baki:auth:revoked"

// revocationMessage is the wire form of one fanned-out revocation.
type revocationMessage struct {
	JTI       string    `json:"jti"`
	ExpiresAt time.Time `json:"expires_at"`
}

const (
	defaultReloadInterval = 30 * time.Second
	// defaultSourceTimeout bounds each lookup, write and publish against the
	// source — the same 3 s the Postgres store used to apply to every request.
	defaultSourceTimeout = 3 * time.Second
	// loadTimeout bounds one LoadActive. The active set is at most the
	// logouts and revocations within one access-token TTL, so this is ample.
	loadTimeout     = 15 * time.Second
	minRetryBackoff = 500 * time.Millisecond
	maxRetryBackoff = 10 * time.Second
	stopDrainBudget = 5 * time.Second
	// defaultMaxTTL caps the expiry a peer-published revocation may carry (see
	// CachedBlacklistOptions.MaxTTL): comfortably above the longest entry this
	// code base writes (24 h refresh TTL; PAT revocations and tickets shorter).
	defaultMaxTTL = 8 * 24 * time.Hour
	// maxMessageBytes and maxJTIBytes bound a peer-published message (a real
	// one is ~100 bytes with a UUID JTI).
	maxMessageBytes = 1024
	maxJTIBytes     = 256
)

// CachedBlacklistOptions configures NewCachedBlacklist. The zero value is a
// single-replica cache: no fan-out, defaults for the rest.
type CachedBlacklistOptions struct {
	// Publisher and Subscriber fan revocations out across replicas. Both nil
	// means a single replica, whose local set is authoritative as soon as it is
	// loaded. Several replicas without them is a misconfiguration: a logout on
	// one reaches the others only on their next periodic reload.
	Publisher  RevocationPublisher
	Subscriber RevocationSubscriber
	// Channel defaults to RevocationChannel.
	Channel string
	// ReloadInterval is how often the local set is reconciled against the
	// source (default 30 s) — the safety net for a lost message, not the
	// delivery path.
	ReloadInterval time.Duration
	// SourceTimeout bounds each lookup, write and publish (default 3 s).
	SourceTimeout time.Duration
	// MaxTTL is the longest an entry received from a peer may live locally
	// (default 8 days). Add and AddIfAbsent are trusted callers; a channel
	// message is not: load merges and sweep only drops expired entries, so
	// without the clamp a year-9999 expiry from anything on the Redis network
	// would be a local entry never reconciled away (unbounded growth — it
	// cannot un-revoke anything). Set it at or above the longest TTL ever
	// passed to Add.
	MaxTTL time.Duration
}

// CachedBlacklist is a BlacklistStore that answers IsRevoked from a local copy
// of the revocation table instead of a round trip per request. The hot path
// (Manager.Verify on every authenticated request, plus the SSE and WebSocket
// revalidation ticks) used to be one SELECT per call; revocations are rare and
// the active set is tiny, so the table is mirrored in memory: every write goes
// to the source first, then into the local set, then — with a Publisher — to
// the peer replicas, which insert it on receipt. A periodic full reload
// reconciles the local set against the source and sweeps expired entries.
//
// The local set is consulted first and a hit is always final: it only ever
// holds entries written to the source (here or on a peer). A miss is final
// only while the set is authoritative — loaded, and either single-replica or
// currently subscribed. Until the first load succeeds, and while a
// subscription is down, a miss falls through to the source on the caller's
// behalf, failing closed on error exactly as the Postgres store does.
type CachedBlacklist struct {
	source  RevocationSource
	pub     RevocationPublisher
	sub     RevocationSubscriber
	channel string

	reloadInterval time.Duration
	sourceTimeout  time.Duration
	maxTTL         time.Duration
	// retryBackoff bounds the boot-load and resubscribe retries; tests
	// shorten it.
	minBackoff, maxBackoff time.Duration

	mu      sync.RWMutex
	entries map[string]time.Time

	// ready is set once a LoadActive has succeeded; subscribed while the peer
	// subscription is established AND the catch-up load that follows it has
	// succeeded. Together they decide whether a local miss is final (see local).
	ready      atomic.Bool
	subscribed atomic.Bool
	// subMu guards subGen, the generation of the current subscription attempt.
	// The catch-up load runs off the receive path, so it may finish after the
	// subscription it belongs to has broken: it only marks subscribed when its
	// generation is still the live one.
	subMu  sync.Mutex
	subGen uint64

	lifeMu    sync.Mutex
	stopped   bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	reload    lifecycle.TickerLoop
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewCachedBlacklist fronts source with a local copy. Call Start to load it
// and to begin following the peers; before that every miss goes to the source.
func NewCachedBlacklist(source RevocationSource, opts CachedBlacklistOptions) *CachedBlacklist {
	bl := &CachedBlacklist{
		source:         source,
		pub:            opts.Publisher,
		sub:            opts.Subscriber,
		channel:        opts.Channel,
		reloadInterval: opts.ReloadInterval,
		sourceTimeout:  opts.SourceTimeout,
		maxTTL:         opts.MaxTTL,
		minBackoff:     minRetryBackoff,
		maxBackoff:     maxRetryBackoff,
		entries:        make(map[string]time.Time),
	}
	if bl.channel == "" {
		bl.channel = RevocationChannel
	}
	if bl.reloadInterval <= 0 {
		bl.reloadInterval = defaultReloadInterval
	}
	if bl.sourceTimeout <= 0 {
		bl.sourceTimeout = defaultSourceTimeout
	}
	if bl.maxTTL <= 0 {
		bl.maxTTL = defaultMaxTTL
	}
	return bl
}

// Start loads the local set (retrying with backoff until the source answers),
// begins the periodic reload, and — with a Subscriber — follows the peers.
// Everything stops when ctx is cancelled or Stop is called. Idempotent; a
// no-op after Stop (the loops it would start could never be stopped again).
func (bl *CachedBlacklist) Start(ctx context.Context) {
	bl.startOnce.Do(func() {
		bl.lifeMu.Lock()
		if bl.stopped {
			bl.lifeMu.Unlock()
			slog.Warn("blacklist cache: Start after Stop ignored")
			return
		}
		ctx, bl.cancel = context.WithCancel(ctx)
		bl.lifeMu.Unlock()

		bl.wg.Add(1)
		go bl.boot(ctx)
		// The reload loop runs from the start: a tick before the boot load
		// has succeeded is simply another attempt at it.
		bl.reload.Start(bl.reloadInterval, false, func(tickCtx context.Context) {
			if err := bl.load(tickCtx); err != nil && tickCtx.Err() == nil {
				slog.Warn("blacklist reload failed; serving the previous copy", "err", err)
			}
		}, func(r any) {
			slog.Error("blacklist reload loop stopped after a panic", "err", r)
		})
		if bl.sub != nil {
			bl.wg.Add(1)
			go bl.subscribeLoop(ctx)
		}
	})
}

// Stop ends the background work, waits (bounded) for it, and stops the
// source. Safe without a prior Start, and idempotent.
func (bl *CachedBlacklist) Stop() {
	bl.stopOnce.Do(func() {
		bl.lifeMu.Lock()
		bl.stopped = true
		cancel := bl.cancel
		bl.lifeMu.Unlock()
		if cancel != nil {
			cancel()
		}
		bl.reload.Stop()
		done := make(chan struct{})
		go func() { bl.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(stopDrainBudget):
			slog.Warn("blacklist background work did not stop within budget")
		}
		bl.source.Stop()
	})
}

// Add revokes jti until ttl elapses: source first, then the local set, then
// the peers.
func (bl *CachedBlacklist) Add(jti string, ttl time.Duration) {
	expiresAt := time.Now().Add(ttl)
	bl.source.Add(jti, ttl)
	bl.insert(jti, expiresAt)
	bl.publish(jti, expiresAt)
}

// AddIfAbsent is the single-use check (see BlacklistStore). The source decides
// — the only atomic view across replicas — and on a source error nothing is
// inserted locally either, so the caller's fail-closed rejection is not
// undermined by a later local hit the source never recorded.
func (bl *CachedBlacklist) AddIfAbsent(jti string, ttl time.Duration) (bool, error) {
	expiresAt := time.Now().Add(ttl)
	added, err := bl.source.AddIfAbsent(jti, ttl)
	if err != nil {
		return false, err
	}
	// Present either way now; an "already present" entry's real expiry is
	// unknown here, and overstating it is harmless (the reload corrects it).
	bl.insert(jti, expiresAt)
	if added {
		bl.publish(jti, expiresAt)
	}
	return added, nil
}

// IsRevoked answers from the local set when it is authoritative, otherwise
// asks the source with a bounded deadline and fails closed on error.
func (bl *CachedBlacklist) IsRevoked(jti string) bool {
	if bl.lookup(jti) {
		return true
	}
	if bl.local() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), bl.sourceTimeout)
	defer cancel()
	revoked, err := bl.source.IsRevokedCtx(ctx, jti)
	if err != nil {
		slog.Warn("blacklist check failed — treating as revoked (fail-closed)", "jti", jti, "err", err)
		return true
	}
	return revoked
}

// IsRevokedCtx is IsRevoked on the caller's context, returning a source error
// instead of deciding for the caller.
func (bl *CachedBlacklist) IsRevokedCtx(ctx context.Context, jti string) (bool, error) {
	if bl.lookup(jti) {
		return true, nil
	}
	if bl.local() {
		return false, nil
	}
	return bl.source.IsRevokedCtx(ctx, jti)
}

// local reports whether a local miss is final: the set has been loaded, and
// nothing published since could have been missed (no peers, or subscribed
// with the post-subscribe catch-up load done).
func (bl *CachedBlacklist) local() bool {
	return bl.ready.Load() && (bl.sub == nil || bl.subscribed.Load())
}

func (bl *CachedBlacklist) lookup(jti string) bool {
	bl.mu.RLock()
	expiresAt, ok := bl.entries[jti]
	bl.mu.RUnlock()
	return ok && time.Now().Before(expiresAt)
}

// insert records jti locally, never shortening an existing entry.
func (bl *CachedBlacklist) insert(jti string, expiresAt time.Time) {
	bl.mu.Lock()
	if expiresAt.After(bl.entries[jti]) {
		bl.entries[jti] = expiresAt
	}
	bl.mu.Unlock()
}

// sweep drops expired entries. Lookups already ignore them; this bounds the
// set's size.
func (bl *CachedBlacklist) sweep() {
	now := time.Now()
	bl.mu.Lock()
	for jti, expiresAt := range bl.entries {
		if !now.Before(expiresAt) {
			delete(bl.entries, jti)
		}
	}
	bl.mu.Unlock()
}

// load reconciles the local set against the source's active entries and marks
// the set ready. It MERGES rather than replaces: an entry that arrived here
// (own Add, or a peer's message) after the source's snapshot was taken would
// be lost by a swap, and nothing in the set is ever wrong — only expired,
// which sweep handles.
func (bl *CachedBlacklist) load(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	active, err := bl.source.LoadActive(ctx)
	if err != nil {
		return err
	}
	bl.sweep()
	bl.mu.Lock()
	for jti, expiresAt := range active {
		if expiresAt.After(bl.entries[jti]) {
			bl.entries[jti] = expiresAt
		}
	}
	bl.mu.Unlock()
	bl.ready.Store(true)
	return nil
}

// boot retries the first load with backoff until it succeeds or ctx ends.
// Misses keep going to the source meanwhile (see IsRevoked).
func (bl *CachedBlacklist) boot(ctx context.Context) {
	defer bl.wg.Done()
	backoff := bl.minBackoff
	for ctx.Err() == nil {
		err := bl.load(ctx)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("blacklist initial load failed; retrying", "err", err, "in", backoff)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, bl.maxBackoff)
	}
}

// subscribeLoop keeps a subscription to the peers' revocations open,
// resubscribing with backoff when it breaks. The set is authoritative only
// while subscribed, and "subscribed" is NOT the server's confirmation: it is
// set once the full load that follows every (re)subscribe has succeeded, so a
// revocation published while this replica was deaf is in the local set before
// a miss is treated as final. Until then misses go to the source.
func (bl *CachedBlacklist) subscribeLoop(ctx context.Context) {
	defer bl.wg.Done()
	backoff := bl.minBackoff
	for ctx.Err() == nil {
		var established atomic.Bool
		gen := bl.currentGen()
		err := bl.sub.Subscribe(ctx, bl.channel, func() {
			established.Store(true)
			// Off the receive path: a slow source must not stall delivery.
			bl.wg.Add(1)
			go bl.catchUp(ctx, gen)
		}, bl.apply)
		bl.endSubscription()
		if ctx.Err() != nil {
			return
		}
		if established.Load() {
			backoff = bl.minBackoff
		}
		slog.Warn("blacklist subscription lost; falling back to the source until resubscribed", "err", err, "in", backoff)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, bl.maxBackoff)
	}
}

// catchUp loads the source after subscription gen was confirmed, retrying with
// backoff, and marks the set subscribed on success — unless gen has been
// superseded (the subscription broke), in which case the next attempt owns
// the flag.
func (bl *CachedBlacklist) catchUp(ctx context.Context, gen uint64) {
	defer bl.wg.Done()
	backoff := bl.minBackoff
	for ctx.Err() == nil && bl.currentGen() == gen {
		err := bl.load(ctx)
		if err == nil {
			bl.subMu.Lock()
			if bl.subGen == gen {
				bl.subscribed.Store(true)
			}
			bl.subMu.Unlock()
			return
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("blacklist reload after subscribe failed; misses stay on the source", "err", err, "retry_in", backoff)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, bl.maxBackoff)
	}
}

func (bl *CachedBlacklist) currentGen() uint64 {
	bl.subMu.Lock()
	defer bl.subMu.Unlock()
	return bl.subGen
}

// endSubscription records that the current subscription attempt is over:
// misses go back to the source, and a catch-up load still running for it can
// no longer mark the set subscribed.
func (bl *CachedBlacklist) endSubscription() {
	bl.subMu.Lock()
	bl.subGen++
	bl.subscribed.Store(false)
	bl.subMu.Unlock()
}

// apply inserts one peer-published revocation. The channel is shared with
// whatever else can reach Redis, so the message is validated as untrusted
// input: bounded size and id, a real future expiry, and an expiry clamped to
// MaxTTL so the entry is always swept eventually.
func (bl *CachedBlacklist) apply(payload []byte) {
	if len(payload) > maxMessageBytes {
		slog.Warn("blacklist: ignoring oversized revocation message", "bytes", len(payload))
		return
	}
	var msg revocationMessage
	if err := json.Unmarshal(payload, &msg); err != nil || msg.JTI == "" || len(msg.JTI) > maxJTIBytes {
		slog.Warn("blacklist: ignoring malformed revocation message", "err", err, "jti_len", len(msg.JTI))
		return
	}
	now := time.Now()
	if msg.ExpiresAt.IsZero() || !msg.ExpiresAt.After(now) {
		return // nothing to revoke: already expired (or no expiry at all)
	}
	if latest := now.Add(bl.maxTTL); msg.ExpiresAt.After(latest) {
		msg.ExpiresAt = latest
	}
	bl.insert(msg.JTI, msg.ExpiresAt)
}

// publish fans one revocation out. A failure is logged, not returned: the
// local and source writes have already happened; peers reconcile on their
// next reload.
func (bl *CachedBlacklist) publish(jti string, expiresAt time.Time) {
	if bl.pub == nil {
		return
	}
	payload, err := json.Marshal(revocationMessage{JTI: jti, ExpiresAt: expiresAt.UTC()})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), bl.sourceTimeout)
	defer cancel()
	if err := bl.pub.Publish(ctx, bl.channel, payload); err != nil {
		slog.Warn("blacklist publish failed; peers reconcile on their next reload", "jti", jti, "err", err)
	}
}

// sleepCtx waits d or until ctx ends, reporting whether the full wait elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

var _ BlacklistStore = (*CachedBlacklist)(nil)
