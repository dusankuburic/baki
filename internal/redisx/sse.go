package redisx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"
	"pad-core/logger"

	"pad-analyzer/internal/metrics"
)

// The SSE backplane (docs/adr/0030-cross-replica-commands-and-sse-replay.md) gives a replica three things the
// in-process event manager cannot: delivery to a client connected to another replica (one channel), event ids a
// reconnecting client can resume from (a short per-user log), and a way to ask whether anyone still listens for a
// user (a per-user hash of the replicas that hold a stream). api.EventManager owns the local half and drives this;
// every part fails open: Redis away means local delivery, frames without an id and "someone may be listening".

// The three families. Owner is the package whose behaviour they serve; the code lives here so the key literals
// stay in one file and the api package never spells a key.
var (
	// SSEEmitChannel carries one event for the clients of other replicas. A message missed during an outage is not
	// replayed from here: a reconnecting client replays from its user's log.
	SSEEmitChannel = Declare(Spec{
		Name:    "sse.emit",
		Kind:    KindPubSub,
		Pattern: "baki:sse:ev",
		Version: sseVersion,
		OnDown:  OnDownOpen,
		Owner:   "internal/api",
	})
	// SSELogKey + user is a Redis Stream of the user's recent durable frames. The entry id is the SSE event id.
	// Trimmed to about SSELogMaxLen entries and expiring SSELogTTL after the last write.
	SSELogKey = Declare(Spec{
		Name:    "sse.log",
		Kind:    KindStream,
		Pattern: "baki:sse:log:{user}",
		TTL:     SSELogTTL,
		Version: sseVersion,
		OnDown:  OnDownOpen,
		Owner:   "internal/api",
	})
	// SSESubsKey + user is a hash: field = replica instance id, value = {"v","kind","n","exp"}. Every record carries
	// its own expiry (the only thing that prunes it); the key expires SSESubsTTL after the last write.
	SSESubsKey = Declare(Spec{
		Name:    "sse.subs",
		Kind:    KindHash,
		Pattern: "baki:sse:subs:{user}",
		TTL:     SSESubsTTL,
		Version: sseVersion,
		OnDown:  OnDownOpen,
		Owner:   "internal/api",
	})
)

const (
	sseVersion  = 1
	sseEmitKind = "sse.emit"
	sseSubsKind = "sse.subs"

	// SSELogTTL is how long a user's log outlives its last write, and the window a reconnect may replay from.
	SSELogTTL = 5 * time.Minute
	// SSELogMaxLen is the approximate length the log is trimmed to.
	SSELogMaxLen = 200
	// SSEReplayMax is the most entries one reconnect replays.
	SSEReplayMax = 200
	// SSEReplayWindow is how far back a reconnect may replay, whatever the id it sends.
	SSEReplayWindow = SSELogTTL
	// SSEMaxLoggedBytes bounds one logged frame: the log holds up to SSELogMaxLen of them per user for SSELogTTL, so
	// a document-sized frame would be megabytes of Redis per user. A larger frame is delivered without an id.
	SSEMaxLoggedBytes = 32 << 10
	// SSEMaxFrameBytes bounds the body of a frame taken from Redis (the api's own frame bound).
	SSEMaxFrameBytes = 256 << 10
	// sseLogMaxUsers bounds the users one event is logged for; the rest of a larger audience gets the frame without
	// an id.
	sseLogMaxUsers = 256
	// sseLogMaxPipelineBytes bounds the frame bytes one append pipeline carries (users x frame size).
	sseLogMaxPipelineBytes = 2 << 20

	// SSERecordTTL is how long a subscriber record is valid after its write; the writer refreshes every
	// SSEPresenceInterval, so a replica that dies stops counting within SSERecordTTL.
	SSERecordTTL = 45 * time.Second
	// SSESubsTTL is the expiry of the whole hash.
	SSESubsTTL = time.Minute
	// SSEPresenceInterval is how often a replica rewrites its records.
	SSEPresenceInterval = 15 * time.Second

	sseLogTimeout        = 250 * time.Millisecond
	sseReplayTimeout     = time.Second
	ssePeerTimeout       = 200 * time.Millisecond
	ssePresenceWriteWait = 2 * time.Second
	ssePublishWait       = 2 * time.Second
	ssePublishDrain      = 2 * time.Second
	ssePublishQueue      = 1024
	ssePublishQueueBytes = 16 << 20
	// sseErrorCache is how long a failed presence read is remembered, so a hung Redis costs one bounded wait per
	// user and second, not one per call.
	sseErrorCache = time.Second
	// sseHintAge is how old a cached answer may be before PeerHint refreshes it in the background.
	sseHintAge      = time.Second
	ssePeerCacheMax = 4096
	sseLogEvery     = 10 * time.Second
)

var (
	sseFanoutTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_sse_fanout_total",
		Help: "SSE events crossing replicas, by direction and outcome. publish: ok, failed (Redis refused it), skipped (Redis degraded, or no other replica holds the user's stream: chunks and progress are published only then), dropped (the publish queue was full). receive: ok (delivered to this replica's clients), self (this replica's own message, filtered), ignored (a version, kind or shape this release does not use).",
	}, []string{"direction", "outcome"})
	sseLogTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_sse_log_total",
		Help: "Durable SSE frames appended to the per-user replay log, by outcome: ok (the frame carries an event id), failed (Redis refused it: delivered without an id), skipped (Redis degraded), oversize (over the logged-frame bound: delivered without an id), truncated (past the per-event audience bound: delivered without an id).",
	}, []string{"outcome"})
	sseReplayTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_sse_replay_total",
		Help: "Reconnects that sent Last-Event-ID, by outcome: ok (frames replayed), empty (nothing newer in the log), failed (Redis could not be read: the stream continues without a replay), ignored (the id was not a stream id), gap (more frames were missed than the log replays, or the log cannot vouch for the id because it is older than the log's window or first entry: the client is told to refetch instead of getting a partial replay), cut (a replay ran out of write budget and the rest became a gap).",
	}, []string{"outcome"})
	sseReplayFrames = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_sse_replay_frames_total",
		Help: "Frames replayed to reconnecting SSE clients from the per-user log.",
	})
	ssePresenceTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pad_sse_presence_reads_total",
		Help: "Reads of whether another replica holds a user's SSE stream, by outcome: hit (answered from the short cache), miss (read from Redis), failed (Redis could not be read: callers treat the user as having a subscriber), degraded (Redis marked degraded: not asked).",
	}, []string{"outcome"})
)

func init() {
	metrics.MustRegister(sseFanoutTotal, sseLogTotal, sseReplayTotal, sseReplayFrames, ssePresenceTotal)
}

// errSSEUnreadable is what a presence read answers when Redis cannot: the caller decides what "unknown" means
// (the chat watchdog: someone may be listening).
var errSSEUnreadable = errors.New("redisx: SSE subscriber presence unreadable")

// SSETarget is one user an event is for, with the id of the frame in that user's log ("" when it was not logged).
type SSETarget struct {
	User string `json:"u"`
	ID   string `json:"id,omitempty"`
}

// SSEMessage is one event on baki:sse:ev: the encoded event body (the JSON of the SSE data line, no framing)
// and who it is for. The origin has already delivered to its own clients; every other replica delivers to its own.
type SSEMessage struct {
	V      int    `json:"v"`
	Kind   string `json:"kind"`
	Origin string `json:"o"`
	// Name is the event name, for the receiver's shedding rule.
	Name string `json:"name"`
	// All delivers to every client of the receiving replica (EmitToAll); otherwise To names the users.
	All  bool            `json:"all,omitempty"`
	To   []SSETarget     `json:"to,omitempty"`
	Data json.RawMessage `json:"d"`
}

// SSEEntry is one replayed log entry.
type SSEEntry struct {
	ID   string
	Data []byte
}

type sseSubsRecord struct {
	V    int    `json:"v"`
	Kind string `json:"kind"`
	N    int    `json:"n"`
	Exp  int64  `json:"exp"` // unix milliseconds
}

type peerEntry struct {
	held       bool
	failed     bool
	at         time.Time
	refreshing atomic.Bool
}

type queuedMessage struct {
	msg  SSEMessage
	size int64
}

// SSEBackplane is one replica's SSE view of Redis. A nil client is not valid: the caller picks the in-process
// event manager when there is none.
type SSEBackplane struct {
	c        *redis.Client
	h        *Health
	instance string

	// now is the clock; tests move it.
	now func() time.Time

	queue       chan queuedMessage
	queuedBytes atomic.Int64
	subscribed  atomic.Bool

	peers struct {
		mu sync.Mutex
		m  map[string]*peerEntry
	}

	lastWarn atomic.Int64
}

// NewSSEBackplane builds the replica's backplane. instance is the replica's id (PAD_INSTANCE_ID), unique among the
// replicas; health is the client's Health (nil is fine: never degraded).
func NewSSEBackplane(c *redis.Client, h *Health, instance string) *SSEBackplane {
	b := &SSEBackplane{c: c, h: h, instance: instance, now: time.Now, queue: make(chan queuedMessage, ssePublishQueue)}
	b.peers.m = map[string]*peerEntry{}
	return b
}

// Instance is the replica id events are published under.
func (b *SSEBackplane) Instance() string { return b.instance }

// Subscribed reports whether the subscription to baki:sse:ev is confirmed right now.
func (b *SSEBackplane) Subscribed() bool { return b.subscribed.Load() }

// warn logs at most once per sseLogEvery.
func (b *SSEBackplane) warn(msg string, kv ...any) {
	now := time.Now().UnixNano()
	last := b.lastWarn.Load()
	if now-last < int64(sseLogEvery) || !b.lastWarn.CompareAndSwap(last, now) {
		return
	}
	logger.Warn(msg, kv...)
}

// ValidStreamID reports whether id is a Redis stream id, "<ms>-<seq>", both parts 64-bit decimals. It is the only
// shape a Last-Event-ID or a replayed id may have: the id is written into the SSE frame, so anything else (a line
// break) must never reach it.
func ValidStreamID(id string) bool {
	_, _, ok := parseStreamID(id)
	return ok
}

func parseStreamID(id string) (ms, seq uint64, ok bool) {
	a, b, found := strings.Cut(id, "-")
	if !found || a == "" || b == "" || len(a) > 20 || len(b) > 20 {
		return 0, 0, false
	}
	for _, s := range []string{a, b} {
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return 0, 0, false
			}
		}
	}
	ms, err1 := strconv.ParseUint(a, 10, 64)
	seq, err2 := strconv.ParseUint(b, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return ms, seq, true
}

// CompareStreamIDs orders two valid stream ids: -1, 0 or 1. An invalid id sorts before a valid one.
func CompareStreamIDs(a, b string) int {
	am, as, aok := parseStreamID(a)
	bm, bs, bok := parseStreamID(b)
	switch {
	case !aok && !bok:
		return 0
	case !aok:
		return -1
	case !bok:
		return 1
	case am != bm:
		if am < bm {
			return -1
		}
		return 1
	case as != bs:
		if as < bs {
			return -1
		}
		return 1
	}
	return 0
}

// Log appends data (the JSON body of one frame) to each user's log in one round trip and returns the entry ids,
// aligned with users; "" means that user's frame was not logged (too large, Redis degraded or failed, or past the
// per-event user bound) and goes out without an id. It runs in the emitter's path, so it is bounded to
// sseLogTimeout and skips Redis entirely while Health says it is away.
func (b *SSEBackplane) Log(ctx context.Context, users []string, data []byte) []string {
	ids := make([]string, len(users))
	if len(users) == 0 {
		return ids
	}
	if len(data) > SSEMaxLoggedBytes {
		sseLogTotal.WithLabelValues("oversize").Add(float64(len(users)))
		return ids
	}
	if b.h.Degraded() {
		sseLogTotal.WithLabelValues("skipped").Add(float64(len(users)))
		return ids
	}
	// Bounded in users and in bytes: one pipeline holds a copy of the frame per user.
	maxUsers := min(sseLogMaxUsers, max(1, sseLogMaxPipelineBytes/max(1, len(data))))
	logged := users
	if len(logged) > maxUsers {
		logged = logged[:maxUsers]
		sseLogTotal.WithLabelValues("truncated").Add(float64(len(users) - maxUsers))
	}
	ctx, cancel := context.WithTimeout(ctx, sseLogTimeout)
	defer cancel()
	cmds := make([]*redis.StringCmd, len(logged))
	_, _ = b.c.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for i, u := range logged {
			key := SSELogKey.Key(u)
			cmds[i] = p.XAdd(ctx, &redis.XAddArgs{Stream: key, MaxLen: SSELogMaxLen, Approx: true, Values: map[string]any{"v": sseVersion, "d": data}})
			p.Expire(ctx, key, SSELogTTL)
		}
		return nil
	})
	var failed int
	for i, cmd := range cmds {
		id, err := cmd.Result()
		if err != nil || !ValidStreamID(id) {
			failed++
			continue
		}
		ids[i] = id
	}
	sseLogTotal.WithLabelValues("ok").Add(float64(len(logged) - failed))
	if failed > 0 {
		sseLogTotal.WithLabelValues("failed").Add(float64(failed))
		b.warn("SSE: replay log append failed; frames delivered without an event id", "users", len(logged), "failed", failed)
	}
	return ids
}

// ReplayGap is what Replay returns instead of a partial replay: the log cannot show the client every frame it missed
// (more were missed than one replay carries, or the cursor is older than the log's window, so entries between are
// gone). The caller tells the client to refetch, and TailID is the newest entry in the log now ("" when the log is
// empty): the marker frame carries it as its id, so the client's cursor moves past what it is about to refetch and
// the next reconnect does not replay the same gap.
type ReplayGap struct{ TailID string }

func (g *ReplayGap) Error() string { return "redisx: SSE replay has a gap; the client must refetch" }

// Replay returns the log entries after afterID, oldest first, when the log holds every one of them: at most
// SSEReplayMax, none older than SSEReplayWindow. When it cannot (a *ReplayGap error) it returns nothing, because a
// prefix of the missed frames followed by the live ones would hide the hole in the middle. An afterID that is not a
// stream id replays nothing (and is not an error): the client sent something this server never issued. Any other
// error means Redis could not be read; the caller continues without a replay.
func (b *SSEBackplane) Replay(ctx context.Context, user, afterID string) ([]SSEEntry, error) {
	ms, seq, ok := parseStreamID(afterID)
	if !ok || seq == ^uint64(0) {
		sseReplayTotal.WithLabelValues("ignored").Inc()
		return nil, nil
	}
	if b.h.Degraded() {
		sseReplayTotal.WithLabelValues("failed").Inc()
		return nil, errSSEUnreadable
	}
	ctx, cancel := context.WithTimeout(ctx, sseReplayTimeout)
	defer cancel()
	key := SSELogKey.Key(user)
	if floor := b.now().Add(-SSEReplayWindow).UnixMilli(); floor > 0 && ms < uint64(floor) {
		return nil, b.gap(ctx, key)
	}
	// Redis 6.2 has an exclusive range; computing the next id works on every version. One more than the cap is read
	// to know whether the cap cut anything off.
	start := fmt.Sprintf("%d-%d", ms, seq+1)
	// The log's first entry is read in the same round trip: a cursor older than it names an entry the log no longer
	// holds (trimmed, or lost with a Redis restart), and whatever came between is unknowable.
	var rest, oldest *redis.XMessageSliceCmd
	if _, err := b.c.Pipelined(ctx, func(p redis.Pipeliner) error {
		rest = p.XRangeN(ctx, key, start, "+", SSEReplayMax+1)
		oldest = p.XRangeN(ctx, key, "-", "+", 1)
		return nil
	}); err != nil {
		sseReplayTotal.WithLabelValues("failed").Inc()
		// The class only: go-redis's text carries the server's address.
		return nil, fmt.Errorf("redisx: SSE replay read failed: %s", errorClass(err))
	}
	msgs := rest.Val()
	if first := oldest.Val(); len(first) == 0 || CompareStreamIDs(afterID, first[0].ID) < 0 || len(msgs) > SSEReplayMax {
		return nil, b.gap(ctx, key)
	}
	out := make([]SSEEntry, 0, len(msgs))
	for _, m := range msgs {
		// A reader skips what it cannot use and deletes nothing: another release may have written it.
		if v, ok := m.Values["v"].(string); ok {
			if n, err := strconv.Atoi(v); err != nil || n > sseVersion {
				continue
			}
		}
		d, ok := m.Values["d"].(string)
		if !ok || !usableBody([]byte(d)) || !ValidStreamID(m.ID) {
			continue
		}
		out = append(out, SSEEntry{ID: m.ID, Data: []byte(d)})
	}
	if len(out) == 0 {
		sseReplayTotal.WithLabelValues("empty").Inc()
		return nil, nil
	}
	sseReplayTotal.WithLabelValues("ok").Inc()
	sseReplayFrames.Add(float64(len(out)))
	return out, nil
}

// usableBody reports whether data can be the body of one SSE data line: non-empty, bounded and without a line
// break (a line break would end the event early and let the rest of the text be read as further fields).
func usableBody(data []byte) bool {
	return len(data) > 0 && len(data) <= SSEMaxFrameBytes && !bytes.ContainsAny(data, "\r\n")
}

// SetLocal records how many SSE streams this replica holds for each user: n > 0 writes the record and refreshes the
// hash's expiry, n == 0 removes this replica's field. One round trip; failures are logged and the next refresh
// rewrites (the record's own expiry bounds a missed removal).
func (b *SSEBackplane) SetLocal(ctx context.Context, counts map[string]int) {
	if len(counts) == 0 || b.h.Degraded() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, ssePresenceWriteWait)
	defer cancel()
	exp := b.now().Add(SSERecordTTL).UnixMilli()
	_, err := b.c.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for user, n := range counts {
			key := SSESubsKey.Key(user)
			if n <= 0 {
				p.HDel(ctx, key, b.instance)
				continue
			}
			rec, _ := json.Marshal(sseSubsRecord{V: sseVersion, Kind: sseSubsKind, N: n, Exp: exp})
			p.HSet(ctx, key, b.instance, rec)
			p.Expire(ctx, key, SSESubsTTL)
		}
		return nil
	})
	if err != nil {
		b.warn("SSE: subscriber presence write failed; the next refresh rewrites it", "err", errorClass(err))
	}
}

// PeerHolds reports whether any OTHER replica holds an SSE stream for user. maxAge is how old a cached answer may
// be (the watchdog asks every 15 s and tolerates 5 s). An error means unknown: Redis could not be read, or is
// marked degraded; callers must not read it as "nobody".
func (b *SSEBackplane) PeerHolds(user string, maxAge time.Duration) (bool, error) {
	if e, ok := b.cached(user); ok {
		age := b.now().Sub(e.at)
		if e.failed && age < sseErrorCache {
			ssePresenceTotal.WithLabelValues("hit").Inc()
			return false, errSSEUnreadable
		}
		if !e.failed && age < maxAge {
			ssePresenceTotal.WithLabelValues("hit").Inc()
			return e.held, nil
		}
	}
	return b.readPeers(user)
}

// PeerHint is PeerHolds for the emit path of chat chunks, which runs at 60 Hz in the worker that streams the
// answer and must not wait on Redis: a known user is answered from the cache at once (refreshed in the background
// once it is sseHintAge old), only a user never seen waits for one bounded read. Unknown is false: a chunk is
// published only when another replica is known to hold the stream.
func (b *SSEBackplane) PeerHint(user string) bool {
	e, ok := b.cached(user)
	if !ok {
		held, err := b.readPeers(user)
		return err == nil && held
	}
	if b.now().Sub(e.at) >= sseHintAge && e.refreshing.CompareAndSwap(false, true) {
		go func() {
			defer e.refreshing.Store(false)
			_, _ = b.readPeers(user)
		}()
	}
	return e.held && !e.failed
}

func (b *SSEBackplane) cached(user string) (*peerEntry, bool) {
	b.peers.mu.Lock()
	defer b.peers.mu.Unlock()
	e, ok := b.peers.m[user]
	return e, ok
}

func (b *SSEBackplane) readPeers(user string) (bool, error) {
	if b.h.Degraded() {
		ssePresenceTotal.WithLabelValues("degraded").Inc()
		// Remembered like a failed read, so PeerHint's background refresh of a stale entry runs once a second, not once a call.
		b.remember(user, false, true)
		return false, errSSEUnreadable
	}
	ctx, cancel := context.WithTimeout(context.Background(), ssePeerTimeout)
	defer cancel()
	fields, err := b.c.HGetAll(ctx, SSESubsKey.Key(user)).Result()
	if err != nil {
		ssePresenceTotal.WithLabelValues("failed").Inc()
		b.remember(user, false, true)
		return false, errSSEUnreadable
	}
	ssePresenceTotal.WithLabelValues("miss").Inc()
	nowMs := b.now().UnixMilli()
	held := false
	for instance, raw := range fields {
		if instance == b.instance {
			continue
		}
		var rec sseSubsRecord
		// Expired, unreadable and future-version records are skipped and left where they are: only their own
		// expiry removes them, and another release may be the writer.
		if Decode(sseSubsKind, sseVersion, []byte(raw), &rec) != ResultOK || rec.N <= 0 || rec.Exp <= nowMs {
			continue
		}
		held = true
		break
	}
	b.remember(user, held, false)
	return held, nil
}

func (b *SSEBackplane) remember(user string, held, failed bool) {
	now := b.now()
	b.peers.mu.Lock()
	defer b.peers.mu.Unlock()
	// An entry is never changed after it is stored (readers use it without the lock); a new answer replaces it.
	if _, ok := b.peers.m[user]; !ok && len(b.peers.m) >= ssePeerCacheMax {
		for u, e := range b.peers.m {
			if now.Sub(e.at) > 30*time.Second {
				delete(b.peers.m, u)
			}
		}
		if len(b.peers.m) >= ssePeerCacheMax {
			clear(b.peers.m)
		}
	}
	b.peers.m[user] = &peerEntry{held: held, failed: failed, at: now}
}

// Publish queues msg for the other replicas and returns at once: the emit path never waits on Redis. The queue is
// bounded by count and bytes; a message that does not fit is dropped (counted). One sender keeps the order of one
// replica's events.
func (b *SSEBackplane) Publish(msg SSEMessage) {
	msg.V, msg.Kind, msg.Origin = sseVersion, sseEmitKind, b.instance
	size := int64(len(msg.Data)) + int64(len(msg.To))*48 + 128
	if b.queuedBytes.Add(size) > ssePublishQueueBytes {
		b.queuedBytes.Add(-size)
		sseFanoutTotal.WithLabelValues("publish", "dropped").Inc()
		return
	}
	select {
	case b.queue <- queuedMessage{msg: msg, size: size}:
	default:
		b.queuedBytes.Add(-size)
		sseFanoutTotal.WithLabelValues("publish", "dropped").Inc()
	}
}

// Run publishes what Publish queued and delivers what the other replicas publish, until ctx ends. deliver is called
// from the subscription's goroutine, must not block, and must not publish again. Losing Redis keeps Run alive: the
// subscription is retried with a backoff and Subscribed reports the gap. On exit the queue is drained for a
// moment so the terminal events of a stopping replica still reach the clients of the others.
func (b *SSEBackplane) Run(ctx context.Context, deliver func(SSEMessage)) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.send(ctx)
	}()
	b.receive(ctx, deliver)
	wg.Wait()
}

func (b *SSEBackplane) send(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			deadline := time.Now().Add(ssePublishDrain)
			for {
				select {
				case q := <-b.queue:
					if time.Now().Before(deadline) {
						b.publishOne(context.Background(), q)
					} else {
						b.queuedBytes.Add(-q.size)
					}
				default:
					return
				}
			}
		case q := <-b.queue:
			b.publishOne(ctx, q)
		}
	}
}

func (b *SSEBackplane) publishOne(ctx context.Context, q queuedMessage) {
	defer b.queuedBytes.Add(-q.size)
	if b.h.Degraded() {
		sseFanoutTotal.WithLabelValues("publish", "skipped").Inc()
		return
	}
	payload, err := json.Marshal(q.msg)
	if err != nil {
		sseFanoutTotal.WithLabelValues("publish", "failed").Inc()
		return
	}
	pctx, cancel := context.WithTimeout(ctx, ssePublishWait)
	defer cancel()
	if err := b.c.Publish(pctx, SSEEmitChannel.Key(), payload).Err(); err != nil {
		sseFanoutTotal.WithLabelValues("publish", "failed").Inc()
		b.warn("SSE: publishing to the other replicas failed; their clients miss this event", "err", errorClass(err))
		return
	}
	sseFanoutTotal.WithLabelValues("publish", "ok").Inc()
}

func (b *SSEBackplane) receive(ctx context.Context, deliver func(SSEMessage)) {
	const maxBackoff = 10 * time.Second
	backoff := 500 * time.Millisecond
	ps := NewPubSub(b.c)
	for ctx.Err() == nil {
		err := ps.Subscribe(ctx, SSEEmitChannel.Key(), func() {
			b.subscribed.Store(true)
			backoff = 500 * time.Millisecond
		}, func(payload []byte) { b.handle(payload, deliver) })
		b.subscribed.Store(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.warn("SSE: the replica subscription is down; clients of other replicas miss events until it returns", "err", errorClass(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// handle validates one message of the channel and hands it on. Everything it does not understand is counted and
// dropped: the sender may be a newer release.
func (b *SSEBackplane) handle(payload []byte, deliver func(SSEMessage)) {
	var msg SSEMessage
	if err := json.Unmarshal(payload, &msg); err != nil || msg.V < 0 || msg.V > sseVersion || (msg.Kind != "" && msg.Kind != sseEmitKind) {
		sseFanoutTotal.WithLabelValues("receive", "ignored").Inc()
		return
	}
	if msg.Origin == b.instance {
		sseFanoutTotal.WithLabelValues("receive", "self").Inc()
		return
	}
	if !usableBody(msg.Data) || (!msg.All && len(msg.To) == 0) {
		sseFanoutTotal.WithLabelValues("receive", "ignored").Inc()
		return
	}
	for i := range msg.To {
		// The id is written into the frame: only a stream id may pass.
		if msg.To[i].ID != "" && !ValidStreamID(msg.To[i].ID) {
			msg.To[i].ID = ""
		}
	}
	sseFanoutTotal.WithLabelValues("receive", "ok").Inc()
	deliver(msg)
}

// SSEFanoutCount is the current value of pad_sse_fanout_total for a direction ("publish", "receive") and outcome.
// Exposed for tests; safe for production use.
func SSEFanoutCount(direction, outcome string) float64 {
	m := &dto.Metric{}
	if err := sseFanoutTotal.WithLabelValues(direction, outcome).Write(m); err != nil || m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// PublishHeld is Publish for the events a later one supersedes (chat chunks and heartbeats, progress ticks): they
// leave this replica only for the users another replica is known to hold a stream for, so a replica that streams
// an answer to its own client puts nothing on the channel. Presence the cache does not know yet counts as not held
// (see PeerHint); the client resumes by offset after a gap.
func (b *SSEBackplane) PublishHeld(msg SSEMessage) {
	if msg.All {
		b.Publish(msg)
		return
	}
	held := make([]SSETarget, 0, len(msg.To))
	for _, t := range msg.To {
		if b.PeerHint(t.User) {
			held = append(held, t)
		}
	}
	if len(held) == 0 {
		sseFanoutTotal.WithLabelValues("publish", "skipped").Inc()
		return
	}
	msg.To = held
	b.Publish(msg)
}

// gap counts a replay that cannot be complete and names the newest entry of the user's log. A failure to read the
// tail is not hidden: the marker then carries no id, and the client's cursor stays where it was.
func (b *SSEBackplane) gap(ctx context.Context, key string) error {
	sseReplayTotal.WithLabelValues("gap").Inc()
	g := &ReplayGap{}
	if newest, err := b.c.XRevRangeN(ctx, key, "+", "-", 1).Result(); err == nil && len(newest) == 1 && ValidStreamID(newest[0].ID) {
		g.TailID = newest[0].ID
	}
	return g
}

// CountReplayCut counts a replay that ran out of its write budget and ended in a gap marker.
func CountReplayCut() { sseReplayTotal.WithLabelValues("cut").Inc() }
