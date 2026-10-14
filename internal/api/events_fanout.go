package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"pad-analyzer/internal/redisx"
	"pad-core/logger"
)

// The cross-replica half of the SSE endpoint (docs/adr/0030-cross-replica-commands-and-sse-replay.md). Without a
// fan-out the EventManager is exactly what it was: this process's clients, no event ids, no replay. With one:
//
//   - every EmitTo*/EmitToAll delivers here first and then publishes the encoded event to the other replicas, which
//     deliver to their own clients and never publish again (the origin filter lives in the backplane);
//   - an event of the durable class is appended to its users' replay log first, and the log entry id is the SSE id
//     of the frame, here and everywhere it is delivered;
//   - the replica records how many streams it holds per user, which answers HasSubscriber across replicas and tells
//     the emit path whether a chat chunk needs to leave this process at all.
//
// Every part fails open: Redis away means local delivery, frames without an id, and a subscriber lookup that says
// yes.

// EventFanout is what the EventManager needs of a cross-replica backplane. *redisx.SSEBackplane is the one
// implementation.
type EventFanout interface {
	// Log appends the frame body to each user's replay log and returns the entry id per user ("" where it was not
	// logged).
	Log(ctx context.Context, users []string, data []byte) []string
	// Publish queues msg for the other replicas; PublishHeld does so only for the users another replica holds.
	Publish(msg redisx.SSEMessage)
	PublishHeld(msg redisx.SSEMessage)
	// Replay returns the logged frames after afterID.
	Replay(ctx context.Context, user, afterID string) ([]redisx.SSEEntry, error)
	// PeerHolds reports whether another replica holds a stream for user; an error means unknown.
	PeerHolds(user string, maxAge time.Duration) (bool, error)
	// SetLocal records this replica's stream count per user (0 removes it).
	SetLocal(ctx context.Context, counts map[string]int)
	// Run publishes and delivers until ctx ends.
	Run(ctx context.Context, deliver func(redisx.SSEMessage))
}

var _ EventFanout = (*redisx.SSEBackplane)(nil)

// WithFanout gives the EventManager the Redis fan-out. Start its loops with RunFanout. A nil f is no fan-out.
func WithFanout(f EventFanout) EventManagerOption {
	return func(m *EventManager) {
		if f != nil {
			m.fan = &eventFan{b: f, dirty: map[string]struct{}{}, wake: make(chan struct{}, 1)}
		}
	}
}

// subscriberCacheAge is how stale a cached "does another replica hold this user" may be for HasSubscriber: the chat
// watchdog asks every 15 s per stream.
const subscriberCacheAge = 5 * time.Second

// eventFan is the EventManager's fan-out state.
type eventFan struct {
	b EventFanout

	// presenceEvery is the refresh period of this replica's subscriber records; 0 is redisx.SSEPresenceInterval
	// (tests shorten it).
	presenceEvery time.Duration
	// afterRegister runs in HandleEvents between registering a stream and replaying into it (tests).
	afterRegister func()

	mu    sync.Mutex
	dirty map[string]struct{} // users whose local stream count changed since the last write
	wake  chan struct{}
}

// ephemeral reports whether ev is the kind of event nobody replays and a later one supersedes: chat chunks and
// heartbeats, progress ticks. They get no id and are not logged, and they cross to another replica only when that
// replica holds the user's stream.
func ephemeral(ev Event) bool {
	switch ev.Name {
	case "analysis:progress", "flow:parse-progress":
		return true
	case "chat:event":
		switch chatEventType(ev.Data) {
		case "chunk", "heartbeat":
			return true
		}
	}
	return false
}

// chatEventType is the "type" of a chat:event payload ("" for a shape that has none).
func chatEventType(data any) string {
	switch d := data.(type) {
	case map[string]any:
		s, _ := d["type"].(string)
		return s
	case map[string]string:
		return d["type"]
	}
	return ""
}

// dataOf is the body of an encoded frame ("data: <json>\n\n", see marshalFrame).
func dataOf(frame []byte) []byte { return frame[len("data: ") : len(frame)-2] }

// frameWithID prefixes an encoded frame with its SSE id line.
func frameWithID(id string, frame []byte) []byte {
	out := make([]byte, 0, len(id)+5+len(frame))
	out = append(out, "id: "...)
	out = append(out, id...)
	out = append(out, '\n')
	return append(out, frame...)
}

// frameIDNotAfter reports whether frame carries an id that is not after max: it was replayed already.
func frameIDNotAfter(frame []byte, max string) bool {
	const prefix = "id: "
	if len(frame) < len(prefix) || string(frame[:len(prefix)]) != prefix {
		return false
	}
	end := len(prefix)
	for end < len(frame) && frame[end] != '\n' {
		end++
	}
	return redisx.CompareStreamIDs(string(frame[len(prefix):end]), max) <= 0
}

// log appends a durable event to its users' replay logs and returns the entry id per user, aligned with users; nil
// when the event is not logged (ephemeral, or for everyone: there is no roster to copy it into).
func (f *eventFan) log(ev Event, frame []byte, users []string) []string {
	if ev.all || len(users) == 0 || ephemeral(ev) {
		return nil
	}
	return f.b.Log(context.Background(), users, dataOf(frame))
}

// publish hands the event to the other replicas. Ephemeral events go only where the user's stream is held.
func (f *eventFan) publish(ev Event, frame []byte, users, ids []string) {
	msg := redisx.SSEMessage{Name: ev.Name, Data: dataOf(frame)}
	switch {
	case ev.all:
		msg.All = true
	case len(users) == 0:
		return
	default:
		msg.To = make([]redisx.SSETarget, len(users))
		for i, u := range users {
			msg.To[i].User = u
			if ids != nil {
				msg.To[i].ID = ids[i]
			}
		}
	}
	if !ev.all && ephemeral(ev) {
		f.b.PublishHeld(msg)
		return
	}
	f.b.Publish(msg)
}

// otherReplicaHolds answers HasSubscriber for a user with no stream here. Unknown (Redis unreadable) is true.
func (f *eventFan) otherReplicaHolds(user string) bool {
	held, err := f.b.PeerHolds(user, subscriberCacheAge)
	return err != nil || held
}

// notePresence marks user's stream count as changed; the presence loop writes it. Safe on a nil receiver (no
// fan-out) and never blocks.
func (f *eventFan) notePresence(user string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.dirty[user] = struct{}{}
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *eventFan) presenceTick() time.Duration {
	if f.presenceEvery > 0 {
		return f.presenceEvery
	}
	return redisx.SSEPresenceInterval
}

// replayWrite bounds the replay of one reconnect: it runs before the live loop, so a slow client must not hold it. A var
// for tests.
var replayWrite = 10 * time.Second

// gapEvent is the event a client is sent when its reconnect cannot be replayed whole: it refetches what it shows. A
// plain {name,data} event, so a client that does not know the name ignores it like any other.
const gapEvent = "replay:gap"

// replay writes the frames logged after lastID to w and returns the id of the last one ("" when none). ok is false
// when the connection failed and the handler should return. A Redis failure is not one: the stream just continues
// without a replay.
func (f *eventFan) replay(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, arm func(), user, lastID string) (replayed string, ok bool) {
	if f.afterRegister != nil {
		f.afterRegister()
	}
	entries, err := f.b.Replay(ctx, user, lastID)
	var gap *redisx.ReplayGap
	if errors.As(err, &gap) {
		// More was missed than the log can replay, or the log cannot vouch for the cursor: a prefix would hide the hole.
		return f.writeGap(w, flusher, arm, gap.TailID)
	}
	if err != nil {
		// Redis cannot be read: the stream continues live, and the client is told it may have missed frames (no id: there
		// is no cursor to move it to).
		logger.FromContext(ctx).Warn("SSE: replay unavailable; continuing with the live stream", "err", err)
		return f.writeGap(w, flusher, arm, "")
	}
	if len(entries) == 0 {
		return "", true
	}
	start := time.Now()
	for _, e := range entries {
		if time.Since(start) > replayWrite {
			// The client reads slower than the log fills: what is left is a gap, and the cursor moves to the end of the
			// entries that were fetched, because the client refetches rather than receiving them.
			redisx.CountReplayCut()
			return f.writeGap(w, flusher, arm, entries[len(entries)-1].ID)
		}
		arm()
		frame := make([]byte, 0, len(e.ID)+len(e.Data)+16)
		frame = append(frame, "id: "...)
		frame = append(frame, e.ID...)
		frame = append(frame, "\ndata: "...)
		frame = append(frame, e.Data...)
		frame = append(frame, '\n', '\n')
		if _, err := w.Write(frame); err != nil {
			return replayed, false
		}
		replayed = e.ID
	}
	flusher.Flush()
	return replayed, true
}

// RunFanout runs the Redis side of the EventManager until ctx ends: publishing, the subscription that delivers the
// other replicas' events to this one's clients, and the writes of this replica's subscriber records. It returns
// at once when there is no fan-out. On exit it removes this replica's records, so a clean stop does not leave
// "someone is listening" behind for the minute they would take to expire.
func (m *EventManager) RunFanout(ctx context.Context) {
	if m.fan == nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.fan.b.Run(ctx, m.deliverRemote)
	}()
	m.runPresence(ctx)
	wg.Wait()
}

// deliverRemote delivers an event another replica published to this replica's clients. It never publishes: the
// origin already did, and a copy published again would loop.
func (m *EventManager) deliverRemote(msg redisx.SSEMessage) {
	frame := make([]byte, 0, len(msg.Data)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, msg.Data...)
	frame = append(frame, '\n', '\n')
	if msg.All {
		m.offer(msg.Name, frame, true, nil, nil)
		return
	}
	users := make([]string, len(msg.To))
	ids := make([]string, len(msg.To))
	for i, t := range msg.To {
		users[i], ids[i] = t.User, t.ID
	}
	m.offer(msg.Name, frame, false, users, ids)
}

// presenceBatch bounds the users written in one round trip.
const presenceBatch = 200

// runPresence keeps this replica's subscriber records true: it rewrites a user's record soon after their stream
// count changes (notePresence) and every user's on a timer, which refreshes the expiry and repairs a write that
// failed. Counts are read when written, so writes cannot be reordered into a stale state.
func (m *EventManager) runPresence(ctx context.Context) {
	tick := time.NewTicker(m.fan.presenceTick())
	defer tick.Stop()
	written := map[string]struct{}{} // users with a record from this replica
	for {
		select {
		case <-ctx.Done():
			if len(written) > 0 {
				gone := make(map[string]int, len(written))
				for u := range written {
					gone[u] = 0
				}
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				m.fan.b.SetLocal(rctx, gone)
				cancel()
			}
			return
		case <-m.fan.wake:
			m.flushPresence(ctx, written, false)
		case <-tick.C:
			m.flushPresence(ctx, written, true)
		}
	}
}

// flushPresence writes the changed users' records, or with all every connected user's too.
func (m *EventManager) flushPresence(ctx context.Context, written map[string]struct{}, all bool) {
	m.fan.mu.Lock()
	dirty := m.fan.dirty
	m.fan.dirty = map[string]struct{}{}
	m.fan.mu.Unlock()

	counts := make(map[string]int, len(dirty))
	m.rlockIndexed()
	if all {
		for u, set := range m.byUser {
			counts[u] = len(set)
		}
		for u := range written {
			if _, ok := counts[u]; !ok {
				counts[u] = 0
			}
		}
	}
	for u := range dirty {
		counts[u] = len(m.byUser[u])
	}
	m.clientsMu.RUnlock()

	batch := make(map[string]int, presenceBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		m.fan.b.SetLocal(ctx, batch)
		batch = make(map[string]int, presenceBatch)
	}
	for u, n := range counts {
		batch[u] = n
		if n > 0 {
			written[u] = struct{}{}
		} else {
			delete(written, u)
		}
		if len(batch) >= presenceBatch {
			flush()
		}
	}
	flush()
}

// writeGap sends the replay:gap marker. tailID, when set, is its id: the newest entry the replay accounts for, so the
// client's cursor leaves the gap behind and the next reconnect does not replay it again. Returns the id the live
// loop must filter on, like replay.
func (f *eventFan) writeGap(w http.ResponseWriter, flusher http.Flusher, arm func(), tailID string) (string, bool) {
	arm()
	frame := []byte(`data: {"name":"` + gapEvent + `","data":{}}` + "\n\n")
	if tailID != "" {
		frame = frameWithID(tailID, frame)
	}
	if _, err := w.Write(frame); err != nil {
		return "", false
	}
	flusher.Flush()
	return tailID, true
}
