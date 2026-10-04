package websocket

import (
	"encoding/json"
	"time"
)

// runRoomHeartbeat re-announces a room's users once per heartbeatPeriod until
// the room is deleted (Leave) or the hub closed. One goroutine per room, per
// replica.
//
// It used to ride on every connection's writePump ping — K store reads and K²
// frames per room per period, each round trip on the goroutine that writes
// the socket, so a slow Redis stalled every outgoing message until Send cut
// clients off as slow. The room does the whole round once: one batched write,
// one read, one frame per user.
func (h *Hub) runRoomHeartbeat(r *room) {
	ticker := time.NewTicker(h.heartbeatPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			h.heartbeat(h.liveClients(r))
		}
	}
}

// liveClients snapshots r's connections that are not disconnected — nil once
// r is no longer the hub's room for its flow. A tick that raced the Leave
// that emptied r can run after a new room for the same flow has opened; it
// must not heartbeat that room's clients on r's behalf (its own goroutine
// does; the stale one is about to exit on r.stop).
func (h *Hub) liveClients(r *room) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.rooms[r.flowID] != r {
		return nil
	}
	live := make([]*Client, 0, len(r.clients))
	for c := range r.clients {
		if !c.disconnected.Load() {
			live = append(live, c)
		}
	}
	return live
}

// heartbeat is one presence refresh for the local connections of one room: it
// re-writes each one's shared presence record so the entry's TTL never lapses
// while the client is live (no-op single-replica), and re-announces each of
// their users to the rest of the room with a server-built presence.update.
// The re-announcement is the only thing that re-announces a present user —
// nothing else does short of a rejoin — so it is how a peer recovers from a
// missed join, snapshot race or lost pub/sub message.
//
// One frame per USER, carrying the user's selection (userSelections), not one
// per connection carrying its own: peers key presence by user and take each
// update's selection as the user's whole selection, so per-connection frames
// made a user's tabs flip each other's selections on every peer every period.
// The frame goes to everyone but the user's own connections.
//
// The clients' presenceMu guards the read-selection → announce pair; the
// heartbeat takes the locks twice and holds them across NEITHER store round
// trip (holding them across the round trips stalled selection changes and
// teardown on the whole room during a Redis brownout):
//   - First pass: snapshot each connection's record, one lock at a time; the
//     store round trips then run with no client's lock held.
//   - Second pass: re-take the locks and keep them through the memory read
//     and the broadcast. This restores the invariant a selection change
//     (handleIncoming holds the same lock) relies on: its announcement lands
//     either before the heartbeat reads the selection or after the
//     heartbeat's, never between the two, where the heartbeat's older
//     selection would go out last. The post-round-trip memory read also sees
//     a change made during the round trip, so the newest selection is
//     announced, not a stale one.
//
// A client found disconnected in the second pass is pruned: its cleanup
// completed while the locks were free, its leave has gone out, and
// re-announcing it would resurrect a departed user on its peers.
func (h *Hub) heartbeat(clients []*Client) {
	if len(clients) == 0 {
		return
	}
	flowID := clients[0].flowID

	live := make([]*Client, 0, len(clients))
	records := make(map[string][]byte, len(clients))
	for _, c := range clients {
		c.presenceMu.Lock()
		if !c.disconnected.Load() {
			live = append(live, c)
			if h.presenceW != nil {
				if b, err := json.Marshal(c.presenceRecord()); err == nil {
					records[c.connID] = b
				}
			}
		}
		c.presenceMu.Unlock()
	}
	if len(live) == 0 {
		return
	}

	// Refresh the shared store through the presence writer — coalesced with
	// the connection's pending upserts, off this goroutine — then read the
	// room. Both run with no client's lock held.
	if h.presenceW != nil {
		h.presenceW.enqueue(flowID, records)
	}
	var stored []presenceRecord
	if h.backplane != nil {
		stored = h.backplane.listPresence(flowID)
	}

	for _, c := range live {
		c.presenceMu.Lock()
	}
	defer func() {
		for _, c := range live {
			c.presenceMu.Unlock()
		}
	}()
	announceable := make([]*Client, 0, len(live))
	for _, c := range live {
		if !c.disconnected.Load() {
			announceable = append(announceable, c)
		}
	}
	if len(announceable) == 0 {
		return
	}

	// The users to announce, in first-seen order, and the room's connections
	// — read from memory after the store round trip (see above).
	users := make([]string, 0, len(announceable))
	announce := make(map[string]bool, len(announceable))
	for _, c := range announceable {
		if !announce[c.UserID] {
			announce[c.UserID] = true
			users = append(users, c.UserID)
		}
	}
	members := h.roomTargets(flowID, nil)
	selections := userSelections(members, announceable, stored, announce)

	for _, userID := range users {
		var displayName string
		targets := make([]*Client, 0, len(members))
		for _, m := range members {
			if m.UserID == userID {
				displayName = m.DisplayName
				continue
			}
			targets = append(targets, m)
		}
		if displayName == "" {
			for _, c := range announceable {
				if c.UserID == userID {
					displayName = c.DisplayName
					break
				}
			}
		}
		h.broadcastTo(flowID, Envelope{
			Type:      EventPresenceUpdate,
			FlowID:    flowID,
			UserID:    userID,
			Timestamp: time.Now(),
			Payload: presenceUpdateOut{
				UserID:          userID,
				DisplayName:     displayName,
				SelectedBlockID: selections[userID],
			},
		}, targets)
	}
}

// userSelections returns, for each user in announce, the selection a
// presence.update announces for them: of that user's live connections'
// selections, the one set last (selectionNewer). Local connections (members,
// the room, plus live, those being heartbeated — the same set unless a test
// heartbeats a client that never joined) are read from memory, never behind;
// with a backplane the user's other-replica connections come from stored. A
// store that cannot be read (stored nil) leaves the local answer.
func userSelections(members, live []*Client, stored []presenceRecord, announce map[string]bool) map[string]string {
	sel := make(map[string]string, len(announce))
	at := make(map[string]int64, len(announce))
	take := func(userID, s string, t int64) {
		if !announce[userID] {
			return
		}
		cur, curAt := sel[userID], at[userID]
		if _, seen := at[userID]; !seen || selectionNewer(s, t, cur, curAt) {
			sel[userID], at[userID] = s, t
		}
	}
	local := make(map[string]bool, len(members)+len(live))
	for _, c := range members {
		local[c.connID] = true
		s, t := c.selection()
		take(c.UserID, s, t)
	}
	for _, c := range live {
		if local[c.connID] {
			continue
		}
		local[c.connID] = true
		s, t := c.selection()
		take(c.UserID, s, t)
	}
	now := time.Now().UnixNano()
	for _, rec := range stored {
		// A local connection's record is at best as new as its memory.
		if local[rec.ConnID] || rec.Exp < now {
			continue
		}
		take(rec.UserID, rec.SelectedBlockID, rec.SelectedAt)
	}
	return sel
}
