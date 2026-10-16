package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/drain"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

type Event struct {
	Name string `json:"name"`
	Data any    `json:"data"`
	// TargetUser is the one user whose clients receive the event (service.LocalUser, "": the clients that connected
	// without an identity), unless all is set.
	TargetUser string `json:"-"`
	// all delivers to every client of every user (EmitToAll); never implied by an empty TargetUser.
	all bool
}

// OrgRoster lists who belongs to an organisation: its owner and members, in any order (EmitToOrg dedupes).
type OrgRoster func(ctx context.Context, orgID string) ([]string, error)

// OrgRosterOf resolves rosters from the stored organisation: its owner, who is not necessarily listed as a member,
// and its members.
func OrgRosterOf(orgs interface {
	LoadOrg(ctx context.Context, id string) (*storageif.Organisation, error)
}) OrgRoster {
	return func(ctx context.Context, orgID string) ([]string, error) {
		org, err := orgs.LoadOrg(ctx, orgID)
		if err != nil {
			return nil, err
		}
		if org == nil {
			return nil, storageif.ErrNotFound
		}
		ids := make([]string, 0, len(org.Members)+1)
		ids = append(ids, org.OwnerID)
		for _, m := range org.Members {
			ids = append(ids, m.UserID)
		}
		return ids, nil
	}
}

// EventManagerOption configures an EventManager at construction.
type EventManagerOption func(*EventManager)

// WithOrgRoster is how EmitToOrg resolves an organisation's members; without it EmitToOrg reaches no one.
func WithOrgRoster(r OrgRoster) EventManagerOption {
	return func(m *EventManager) { m.roster = r }
}

// sseHeartbeatInterval is how often HandleEvents writes an SSE comment frame
// on a quiet connection; the client's watchdog is ~2x this — keep in sync.
const sseHeartbeatInterval = 20 * time.Second

// sseWriteTimeout bounds each individual write (no global WriteTimeout — it would kill healthy
// idle connections): a stuck write fails and the cleanup path tears the connection down.
const sseWriteTimeout = 30 * time.Second

// maxFrameBytes bounds one encoded SSE frame: each client buffers up to 512 frames, so unbounded
// frames could pin frame-size x 512 bytes per slow connection. Oversized document events swap to their lightweight form.
const maxFrameBytes = 256 << 10

// maxClientBufferBytes bounds the encoded frame bytes one client's channel may hold queued: the
// 512 slots bound COUNT only — a stalled client could otherwise pin 128 MiB. Exceeding it is a drop.
const maxClientBufferBytes = 8 << 20

// EventManager manages Server-Sent Events (SSE) clients and implements
// service.EventNotifier to allow internal services to emit events to the frontend.
type EventManager struct {
	clients map[chan []byte]string // pre-encoded SSE frame channel → userID ("" in local mode)
	// clientsMu guards clients, byUser, indexed and sseConnCount: connect/disconnect take the write
	// lock, deliver and HasSubscriber the read lock, so one user's chat chunks never wait on another's
	// fan-out. A dropped frame's log write must stay outside it (see deliver).
	clientsMu sync.RWMutex
	// bufBytes tracks each client's queued frame bytes for the per-client byte budget: deliver
	// charges it on offer, the SSE write loop refunds as frames reach the socket.
	bufBytes sync.Map
	// byUser indexes clients by userID so a user-targeted event (chat chunks at ~60/s per stream)
	// touches only that user's channels. clients stays the source of truth — see syncIndexLocked
	// for why the two can diverge.
	byUser       map[string]map[chan []byte]struct{}
	indexed      int
	sseConnCount map[string]int // per-client SSE connection counter (keyed by clientKey)
	shutdownCh   <-chan struct{}
	// allowOrigin/clientKey/isRevoked are written ONLY from NewRouter, which completes before
	// http.Serve (a happens-before edge over handler reads); a post-startup setter would need atomics.
	allowOrigin func(string) bool          // injected by Router; nil = deny all cross-origin
	clientKey   func(*http.Request) string // injected by Router; nil = fall back to remote IP
	isRevoked   func(string) bool          // injected by Router; nil = skip blacklist re-check

	heartbeatInterval time.Duration // test override; 0 ⇒ sseHeartbeatInterval

	// droppedFrames counts frames offerFrame could not enqueue (buffer full); dropWarnedAt is when
	// the last "buffer full" warning was written, rate-limited so a stalled client doesn't log per event.
	droppedFrames atomic.Int64
	dropWarnedAt  atomic.Int64

	// roster resolves EmitToOrg's audience; nil (no server store) reaches no one.
	roster OrgRoster

	// revokers holds, per access-token id, a channel for each stream opened with that token: Revoke closes them, so
	// a logout or a deleted API token ends its streams at once instead of at the 2-minute blacklist check.
	revokeMu sync.Mutex
	revokers map[string]map[chan struct{}]struct{}

	// draining is the server's draining state; nil is the process's (drain.Process), tests set their own.
	draining *drain.State

	// fan is the cross-replica backplane (WithFanout); nil is today's behaviour: this process's clients only, no
	// event ids, no replay.
	fan *eventFan
	// connSlots counts a user's streams across the replicas (WithConnCap); nil is this replica's cap only.
	connSlots *redisx.SlotCap
}

// dropWarnInterval is the minimum gap between two "send buffer full" warnings; the drops in
// between are counted and their total reported in the next line.
const dropWarnInterval = 10 * time.Second

// heartbeatTick returns the SSE heartbeat interval, honouring the test
// override so heartbeat tests don't sleep for real 20s ticks.
func (m *EventManager) heartbeatTick() time.Duration {
	if m.heartbeatInterval > 0 {
		return m.heartbeatInterval
	}
	return sseHeartbeatInterval
}

func NewEventManager(shutdownCh chan struct{}, opts ...EventManagerOption) *EventManager {
	m := &EventManager{
		clients:      make(map[chan []byte]string),
		byUser:       make(map[string]map[chan []byte]struct{}),
		sseConnCount: make(map[string]int),
		shutdownCh:   shutdownCh,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// SetOriginChecker injects the router's CORS logic so the SSE endpoint
// respects the same origin allowlist as every other route.
func (m *EventManager) SetOriginChecker(fn func(string) bool) {
	m.allowOrigin = fn
}

// SetClientKeyFunc injects the router's connection-limit key function: per authenticated user in
// JWT mode, else per proxy-aware client IP — proxied clients don't collapse onto one shared bucket.
func (m *EventManager) SetClientKeyFunc(fn func(*http.Request) string) {
	m.clientKey = fn
}

// Revoke ends, at once, the streams opened with the access token jti (a session's at logout, auth.PATJTI of a
// deleted API token) and returns how many. The periodic blacklist check stays: it covers a revocation made on
// another replica.
func (m *EventManager) Revoke(jti string) int {
	m.revokeMu.Lock()
	defer m.revokeMu.Unlock()
	streams := m.revokers[jti]
	for kick := range streams {
		close(kick)
	}
	delete(m.revokers, jti)
	return len(streams)
}

// SubscribeRevocations ends a revoked token's streams at once (events.AccessRevoked: a logout, a deleted API token).
// Async, after the revocation committed; inline when the bus is saturated, as it only closes channels.
func (m *EventManager) SubscribeRevocations(bus *eventbus.Bus) {
	eventbus.Subscribe(bus, eventbus.Async, "sse-access-revoked", func(_ context.Context, e events.AccessRevoked) {
		m.Revoke(e.JTI)
	}, eventbus.InlineWhenSaturated(), eventbus.Replicated(func(e events.AccessRevoked) []eventbus.Invalidation {
		// The other replicas' streams of the token end from the nudge (recheck.sse-revocation), not at their
		// periodic blacklist check; the scope is the token id, as Revoke takes it.
		if e.JTI == "" {
			return nil
		}
		return []eventbus.Invalidation{{Key: "recheck.sse-revocation", Scope: eventbus.NewScope("jti", e.JTI)}}
	}, "recheck.sse-revocation"))
}

// watchRevocation registers a stream of the token jti; the returned channel closes when Revoke ends it, and stop
// unregisters it.
func (m *EventManager) watchRevocation(jti string) (revoked <-chan struct{}, stop func()) {
	kick := make(chan struct{})
	m.revokeMu.Lock()
	if m.revokers == nil {
		m.revokers = map[string]map[chan struct{}]struct{}{}
	}
	if m.revokers[jti] == nil {
		m.revokers[jti] = map[chan struct{}]struct{}{}
	}
	m.revokers[jti][kick] = struct{}{}
	m.revokeMu.Unlock()
	return kick, func() {
		m.revokeMu.Lock()
		defer m.revokeMu.Unlock()
		if streams := m.revokers[jti]; streams != nil {
			delete(streams, kick)
			if len(streams) == 0 {
				delete(m.revokers, jti)
			}
		}
	}
}

// revocationWatchers is how many open streams the token jti has (tests).
func (m *EventManager) revocationWatchers(jti string) int {
	m.revokeMu.Lock()
	defer m.revokeMu.Unlock()
	return len(m.revokers[jti])
}

// SetRevocationChecker injects the auth manager's blacklist check so the SSE loop can periodically
// re-verify the access token (logout / explicit revoke). Password change and refresh-replay revoke
// only refresh tokens; those sessions end at access-token expiry, enforced directly by the loop.
func (m *EventManager) SetRevocationChecker(fn func(string) bool) {
	m.isRevoked = fn
}

// EmitTo satisfies the service.EventNotifier interface: delivers only to userID's clients. "" is
// service.LocalUser, the clients that connected without an identity (a deployment without accounts), never a
// broadcast: with accounts no client registers under it.
func (m *EventManager) EmitTo(userID, name string, data any) {
	m.deliver(Event{Name: name, Data: data, TargetUser: userID})
}

// EmitToAll satisfies the service.EventNotifier interface: delivers to every connected client of every user.
func (m *EventManager) EmitToAll(name string, data any) {
	m.deliver(Event{Name: name, Data: data, all: true})
}

// EmitToOrg satisfies the service.EventNotifier interface: delivers to every member of orgID (owner included),
// each once, resolving the roster on ctx at delivery. A failed read is logged: the event is best-effort, the
// change it announces already made.
func (m *EventManager) EmitToOrg(ctx context.Context, orgID, name string, data any) {
	if m.roster == nil || orgID == "" {
		return
	}
	ids, err := m.roster(ctx, orgID)
	if err != nil {
		logger.FromContext(ctx).Warn("SSE: organisation roster unavailable; event not delivered", "event", name, "orgId", orgID, "err", err)
		return
	}
	seen := make(map[string]bool, len(ids))
	users := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		users = append(users, id)
	}
	m.deliverTo(Event{Name: name, Data: data}, users)
}

// HasSubscriber satisfies the service.EventNotifier interface: whether EmitTo(userID, …) would reach
// a connected client. Services use it to detect abandoned work — e.g. a chat stream whose tab closed
// without /cancel. With the Redis fan-out a stream held by another replica counts, and a presence that cannot be read
// counts as "has a subscriber": a stream is never cancelled for a lookup that failed.
func (m *EventManager) HasSubscriber(userID string) bool {
	m.rlockIndexed()
	local := len(m.byUser[userID]) > 0
	m.clientsMu.RUnlock()
	if local || m.fan == nil {
		return local
	}
	return m.fan.otherReplicaHolds(userID)
}

// rlockIndexed takes the read lock with byUser guaranteed to cover clients; the index only falls
// behind when a test seeds m.clients directly, and that case upgrades to the write lock.
func (m *EventManager) rlockIndexed() {
	m.clientsMu.RLock()
	if m.indexed == len(m.clients) {
		return
	}
	m.clientsMu.RUnlock()
	m.clientsMu.Lock()
	m.syncIndexLocked()
	m.clientsMu.Unlock()
	m.clientsMu.RLock()
}

// addClientLocked registers ch for userID in clients and the per-user index.
// Caller holds clientsMu.
func (m *EventManager) addClientLocked(ch chan []byte, userID string) {
	m.syncIndexLocked()
	m.bufBytes.Store(ch, new(atomic.Int64))
	m.clients[ch] = userID
	set := m.byUser[userID]
	if set == nil {
		set = make(map[chan []byte]struct{})
		m.byUser[userID] = set
	}
	set[ch] = struct{}{}
	m.indexed++
}

// removeClientLocked unregisters ch, dropping its user's index entry once it
// is empty so departed users do not accumulate. Caller holds clientsMu.
func (m *EventManager) removeClientLocked(ch chan []byte) {
	m.syncIndexLocked()
	userID, ok := m.clients[ch]
	if !ok {
		return
	}
	delete(m.clients, ch)
	m.bufBytes.Delete(ch)
	if set := m.byUser[userID]; set != nil {
		delete(set, ch)
		if len(set) == 0 {
			delete(m.byUser, userID)
		}
	}
	m.indexed--
}

// syncIndexLocked rebuilds byUser when it no longer covers clients: production paths keep them in
// step, but package tests seed m.clients directly, and a stale index would silently withhold events.
func (m *EventManager) syncIndexLocked() {
	if m.indexed == len(m.clients) {
		return
	}
	clear(m.byUser)
	for ch, userID := range m.clients {
		set := m.byUser[userID]
		if set == nil {
			set = make(map[chan []byte]struct{})
			m.byUser[userID] = set
		}
		set[ch] = struct{}{}
	}
	m.indexed = len(m.clients)
}

// Event delivery priority: every event class shares the 512-slot buffer, and analysis progress
// arrives in bursts of hundreds — a burst used to push the buffer full and silently drop chat chunks
// queued behind it. Past the high-water mark (sheddableNum/sheddableDen, two-thirds full) droppable
// events are discarded to keep headroom for what a later message cannot reconstruct.
const (
	sheddableNum = 2
	sheddableDen = 3
)

// sheddable reports whether an event may be dropped early to protect the buffer: progress ticks
// are UI polish superseded by the next; a completion or chat frame is not recoverable this way.
func sheddable(name string) bool {
	switch name {
	case "analysis:progress":
		return true
	default:
		return false
	}
}

func (m *EventManager) deliver(ev Event) {
	if ev.all {
		m.deliverTo(ev, nil)
		return
	}
	m.deliverTo(ev, []string{ev.TargetUser})
}

// deliverTo offers ev to the clients of users, or to every client when ev.all is set.
func (m *EventManager) deliverTo(ev Event, users []string) {
	// Encode the SSE frame ONCE for all recipients: per-client encoding would
	// multiply the cost (and allocations) by the number of live SSE tabs.
	frame, ok := encodeEventFrame(ev)
	if !ok {
		return
	}
	if m.fan == nil {
		m.offer(ev.Name, frame, ev.all, users, nil)
		return
	}
	// With the backplane: log first (the entry id is the frame's SSE id), deliver here, then hand the event to the
	// other replicas, which deliver to their own clients without publishing again.
	ids := m.fan.log(ev, frame, users)
	m.offer(ev.Name, frame, ev.all, users, ids)
	m.fan.publish(ev, frame, users, ids)
}

// offer hands frame to this process's clients: every client when all is set, else those of users. ids, when not
// nil, holds the event id of each user's copy (aligned with users; "" for none), written as the frame's id line.
func (m *EventManager) offer(name string, frame []byte, all bool, users, ids []string) {
	droppable := sheddable(name)

	// Only the map walk runs under the READ lock (a channel send is safe from any number of readers).
	// Drops are counted here and logged after the release — the synchronous log write would stall
	// every delivery and connect if run inside the lock.
	dropped := 0
	if all {
		m.clientsMu.RLock()
		for ch := range m.clients {
			if !m.offerFrame(ch, frame, droppable) {
				dropped++
			}
		}
		m.clientsMu.RUnlock()
	} else {
		// Targeted: only the target users' own channels.
		m.rlockIndexed()
		for i, user := range users {
			set := m.byUser[user]
			if len(set) == 0 {
				continue
			}
			f := frame
			if ids != nil && ids[i] != "" {
				f = frameWithID(ids[i], frame)
			}
			for ch := range set {
				if !m.offerFrame(ch, f, droppable) {
					dropped++
				}
			}
		}
		m.clientsMu.RUnlock()
	}
	if dropped > 0 {
		m.noteDropped(name, dropped)
	}
}

// noteDropped counts n frames that found a client's buffer full and warns at most once per
// dropWarnInterval, with the total since the last warning. Lock-free: called outside clientsMu.
func (m *EventManager) noteDropped(name string, n int) {
	total := m.droppedFrames.Add(int64(n))
	now := time.Now().UnixNano()
	last := m.dropWarnedAt.Load()
	if now-last < int64(dropWarnInterval) {
		return
	}
	if !m.dropWarnedAt.CompareAndSwap(last, now) {
		return // a concurrent delivery just warned
	}
	logger.Warn("SSE client dropped event: send buffer full", "event", name, "droppedTotal", total)
}

// offerFrame enqueues frame on one client's channel without blocking; false means dropped — by
// slots or by the per-client byte budget (a shed droppable frame is not a drop: a later frame supersedes it).
func (m *EventManager) offerFrame(ch chan []byte, frame []byte, droppable bool) bool {
	// Shed low-priority events before the buffer is exhausted so a progress
	// burst cannot starve a chat chunk queued right behind it.
	if droppable && len(ch)*sheddableDen > cap(ch)*sheddableNum {
		return true
	}
	// The byte budget is charged before the send and refunded on failure, so a racing offer can
	// transiently overcount (conservative) but never undercount.
	var queued *atomic.Int64
	if v, ok := m.bufBytes.Load(ch); ok {
		queued = v.(*atomic.Int64)
		if queued.Add(int64(len(frame))) > maxClientBufferBytes {
			queued.Add(-int64(len(frame)))
			return false
		}
	}
	select {
	case ch <- frame:
		return true
	default:
		if queued != nil {
			queued.Add(-int64(len(frame)))
		}
		return false
	}
}

// marshalFrame encodes ev into its "data: …\n\n" SSE wire frame, or nil when
// the JSON marshal fails (already logged).
func marshalFrame(ev Event) []byte {
	data, err := json.Marshal(ev)
	if err != nil {
		logger.Warn("SSE: encode event failed; dropping", "event", ev.Name, "err", err)
		return nil
	}
	frame := append(append(make([]byte, 0, len(data)+16), "data: "...), data...)
	return append(frame, '\n', '\n')
}

// encodeEventFrame marshals ev and enforces the maxFrameBytes bound: an oversized document-carrying
// event is replaced by its lightweight form (consumers refetch over HTTP), anything else is dropped.
func encodeEventFrame(ev Event) ([]byte, bool) {
	frame := marshalFrame(ev)
	if frame == nil {
		return nil, false
	}
	if len(frame) <= maxFrameBytes {
		return frame, true
	}
	origLen := len(frame)
	if light, ok := lightweightEvent(ev); ok {
		if lf := marshalFrame(light); lf != nil && len(lf) <= maxFrameBytes {
			logger.Warn("SSE: event exceeded frame bound; delivered lightweight form instead",
				"event", ev.Name, "bytes", origLen, "deliveredBytes", len(lf))
			return lf, true
		}
		logger.Warn("SSE: event exceeded frame bound and its lightweight form was unusable; dropped",
			"event", ev.Name, "bytes", origLen)
		return nil, false
	}
	logger.Warn("SSE: event exceeded frame bound; dropped", "event", ev.Name, "bytes", origLen)
	return nil, false
}

// lightweightEvent returns a bounded-size replacement for an oversized document-carrying event —
// the backstop for an emission site that regresses to sending the full document.
func lightweightEvent(ev Event) (Event, bool) {
	if ev.Name != "flow:loaded" {
		return Event{}, false
	}
	var doc *models.FlowDocument
	switch d := ev.Data.(type) {
	case *models.FlowDocument:
		doc = d
	case models.FlowDocument:
		doc = &d
	default:
		return Event{}, false
	}
	return Event{Name: ev.Name, Data: service.FlowLoadedSummaryOf(doc), TargetUser: ev.TargetUser, all: ev.all}, true
}

// sseShutdownFlushTimeout bounds the final write of the frames still queued when the server closes SSE: a
// stalled client must not hold the stop.
const sseShutdownFlushTimeout = time.Second

// flushQueued writes the frames already queued on ch, under one sseShutdownFlushTimeout deadline, and stops at
// the first failed write; frames offered after it started are left (the stream is ending).
func flushQueued(w http.ResponseWriter, flusher http.Flusher, rc *http.ResponseController, ch chan []byte, queuedBytes *atomic.Int64) {
	_ = rc.SetWriteDeadline(time.Now().Add(sseShutdownFlushTimeout))
	for n := len(ch); n > 0; n-- {
		frame := <-ch
		if queuedBytes != nil {
			queuedBytes.Add(-int64(len(frame)))
		}
		if _, err := w.Write(frame); err != nil {
			return
		}
	}
	flusher.Flush()
}

// HandleEvents is the HTTP handler for the SSE endpoint.
// @Summary      Server-sent events stream
// @Description  Long-lived SSE channel for analysis, chat, and collaboration events. Chunks stream incrementally. On a deployment with a shared Redis (several replicas) every event except chat chunks, heartbeats and progress ticks carries an `id:` line, and a client that reconnects with Last-Event-ID is replayed the frames it missed (at most 200, from the last 5 minutes) before the live stream continues. A reconnect the log cannot replay whole (more frames missed, a cursor older than the log, or the log unreadable) gets one `replay:gap` event instead of a partial replay, which tells the client to refetch what it shows; without Redis the frames carry no id and the header is ignored, so a client refetches what it needs after a reconnect. While the server is stopping a new stream is refused with 503 and code SERVER_RESTARTING (Retry-After); one already open stays open until the server has delivered the chat streams' terminal events.
// @Tags         realtime
// @Produce      text/event-stream
// @Param        Last-Event-ID header string false "The id of the last frame the client received; replays what followed it (needs the shared Redis, else ignored)"
// @Success      200 {string} string "Event stream"
// @Failure      503 {object} render.ErrorResponse "The server is restarting (code SERVER_RESTARTING; reconnect after Retry-After), or too many connections"
// @Router       /api/events [get]
func (m *EventManager) HandleEvents(w http.ResponseWriter, r *http.Request) {
	// A stopping server takes no new stream: the client's reconnect reaches another replica, or this one restarted.
	if m.draining.Draining() {
		drain.Refuse(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		render.Error(w, fmt.Errorf("streaming unsupported"), http.StatusInternalServerError)
		return
	}

	// Per-write deadline machinery (see sseWriteTimeout). If the underlying writer cannot honor
	// deadlines, that is logged once and writes proceed undelayed rather than tearing down connections.
	rc := http.NewResponseController(w)
	deadlineOK := true
	armWriteDeadline := func() {
		if !deadlineOK {
			return
		}
		if err := rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil {
			logger.FromContext(r.Context()).Warn("SSE: per-write deadlines unsupported by this connection; stalled clients cannot be reaped by write timeout", "err", err)
			deadlineOK = false
		}
	}

	// Key the connection cap per authenticated user (JWT mode) or per proxy-aware client IP, via the
	// Router-injected clientKey. The key is "user:<id>" in JWT mode — strip the prefix so userID
	// matches what services pass to EmitTo; in local mode the key is the bare IP (userID "", service.LocalUser).
	key := middleware.ClientIP(r, nil)
	userID := ""
	if m.clientKey != nil {
		key = m.clientKey(r)
		if strings.HasPrefix(key, "user:") {
			userID = key[5:]
		}
	}

	// The user's streams on every replica count too (WithConnCap); asked before the lock every delivery takes.
	releaseSlot, ok := m.connSlots.Acquire(r.Context(), userID)
	if !ok {
		render.Error(w, fmt.Errorf("too many connections"), http.StatusServiceUnavailable)
		return
	}
	m.clientsMu.Lock()
	if m.sseConnCount[key] >= maxSSEConnsPerClient {
		m.clientsMu.Unlock()
		releaseSlot()
		render.Error(w, fmt.Errorf("too many connections"), http.StatusServiceUnavailable)
		return
	}
	m.sseConnCount[key]++
	// Big enough to absorb bursts; a client that stops reading is bounded by both the 512-slot cap
	// and the per-client byte budget — Emit drops at either bound.
	ch := make(chan []byte, 512)
	m.addClientLocked(ch, userID)
	m.clientsMu.Unlock()
	m.fan.notePresence(userID)

	defer func() {
		m.clientsMu.Lock()
		m.removeClientLocked(ch)
		m.sseConnCount[key]--
		if m.sseConnCount[key] <= 0 {
			delete(m.sseConnCount, key)
		}
		m.clientsMu.Unlock()
		releaseSlot()
		m.fan.notePresence(userID)
		sseClientEnd()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// No-op for the Tauri localhost sidecar, but nginx-style reverse proxies buffer responses by
	// default, which would deliver the whole stream at once instead of incrementally.
	w.Header().Set("X-Accel-Buffering", "no")
	// Respect the configured CORS allowlist instead of wildcarding. Vary: Origin is set
	// unconditionally so a caching proxy can't serve a no-ACAO SSE variant to a later request.
	w.Header().Set("Vary", "Origin")
	if origin := r.Header.Get("Origin"); origin != "" && m.allowOrigin != nil && m.allowOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	// Write the status explicitly before flushing: the first Fprintf would trigger an implicit
	// WriteHeader via the otelhttp wrapper and the Flush would trigger it again (superfluous warning).
	armWriteDeadline()
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sseClientStart()

	// Token JTI and expiry for periodic re-validation; empty in local mode.
	jti := ""
	var tokenExp time.Time
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		jti = claims.ID
		if claims.ExpiresAt != nil {
			tokenExp = claims.ExpiresAt.Time
		}
	}

	ctx := r.Context()

	// queuedBytes refunds the byte budget as frames reach the socket
	// (offerFrame charges it on enqueue).
	var queuedBytes *atomic.Int64
	if v, ok := m.bufBytes.Load(ch); ok {
		queuedBytes = v.(*atomic.Int64)
	}

	// Heartbeat: keep a quiet connection observably alive so the client's watchdog can tell "no
	// events" from "dead socket", and a write failure here ends the handler.
	heartbeat := time.NewTicker(m.heartbeatTick())
	defer heartbeat.Stop()

	// Drop the stream once the access token is revoked: at once when this replica revokes it (Revoke), else when
	// the blacklist says so, checked on a slow ticker because it hits the shared blacklist store.
	var revokeC <-chan time.Time
	var revoked <-chan struct{}
	if jti != "" {
		var stopWatching func()
		revoked, stopWatching = m.watchRevocation(jti)
		defer stopWatching()
	}
	if m.isRevoked != nil && jti != "" {
		revokeTicker := time.NewTicker(2 * time.Minute)
		defer revokeTicker.Stop()
		revokeC = revokeTicker.C
	}

	// Drop the stream at access-token expiry. The frontend reconnects with a freshly-refreshed
	// token (ensureFreshToken); no DB cost, so fire precisely at expiry instead of polling.
	var expiryC <-chan time.Time
	if !tokenExp.IsZero() {
		expiryTimer := time.NewTimer(time.Until(tokenExp))
		defer expiryTimer.Stop()
		expiryC = expiryTimer.C
	}

	// A reconnect that names the last frame it saw is replayed what followed it before the live frames: this
	// stream is already registered, so nothing emitted from here on is missed, and a frame that is both in the
	// replay and queued live is written once (replayed below).
	replayed := ""
	if lastID := strings.TrimSpace(r.Header.Get("Last-Event-ID")); lastID != "" && m.fan != nil {
		var ok bool
		if replayed, ok = m.fan.replay(ctx, w, flusher, armWriteDeadline, userID, lastID); !ok {
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.shutdownCh:
			// The stop ends the chat streams before it closes SSE (ADR 0024): their terminal events may still be
			// queued, and a select with both ready picks at random. Write what is queued first.
			flushQueued(w, flusher, rc, ch, queuedBytes)
			return
		case <-revoked:
			logger.Info("SSE: disconnecting client after token revoked", "jti", jti)
			return
		case <-revokeC:
			if m.isRevoked(jti) {
				logger.Info("SSE: disconnecting client after token revoked", "jti", jti)
				return
			}
		case <-expiryC:
			logger.Info("SSE: disconnecting client after token expired", "jti", jti)
			return
		case <-heartbeat.C:
			// SSE comment frame: the client parser ignores non-"data: " lines,
			// but any bytes reset its inactivity watchdog.
			armWriteDeadline()
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case frame := <-ch:
			// Frame arrives pre-encoded by deliver (one encode per event).
			if queuedBytes != nil {
				queuedBytes.Add(-int64(len(frame)))
			}
			if replayed != "" && frameIDNotAfter(frame, replayed) {
				continue
			}
			armWriteDeadline()
			if _, err := w.Write(frame); err != nil {
				// Dead or stalled socket: return so the goroutine, its
				// channel and its queued frames are released.
				return
			}
			flusher.Flush()
		}
	}
}
