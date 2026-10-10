package websocket

import (
	"context"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
)

// An access change reaches the open sockets here: the hub flags the sockets the change can reach, and each one's
// re-authz loop re-checks its access at once instead of at its next 2-minute pass (Client.runReauthzLoop). The
// event only says where to look; the recheck asks storage and the token blacklist, as the periodic pass does.
// Flagging reads no storage and never waits, and a socket already flagged stays flagged once, so a burst of changes
// costs it one more pass, not one per change. Only this replica's sockets: another replica's sockets learn of the
// change at their periodic pass (ADR 0020).

// SubscribeAccessRechecks re-checks the sockets of h that an access change can reach once the change has committed:
// a flow's sharing changed or the flow was deleted; a membership was removed or its role changed, or the
// organisation was deleted; an account was erased; a credential was revoked. A membership added only widens access
// (the periodic pass raises the write rank it may give); a sharing change is rechecked whichever way it went, as
// the event does not say.
//
// Async: a recheck before the commit would read the old grants and keep the socket, and a rolled-back change must
// reach no one. Flagging is quick and must not be lost, so a saturated bus runs it inline in the committing
// request's post-commit hook rather than dropping it.
func SubscribeAccessRechecks(bus *eventbus.Bus, h *Hub) {
	const name = "ws-access-recheck"
	eventbus.Subscribe(bus, eventbus.Async, name, func(_ context.Context, e events.FlowChanged) {
		if e.Kind == events.FlowSharingChanged || e.Kind == events.FlowDeleted {
			h.RecheckFlow(e.FlowID)
		}
	}, eventbus.InlineWhenSaturated())
	eventbus.Subscribe(bus, eventbus.Async, name, func(_ context.Context, e events.OrgMemberChanged) {
		switch e.Kind {
		case events.OrgMemberRemoved, events.OrgMemberRoleChanged:
			h.RecheckUser(e.UserID)
		case events.OrgDeleted:
			h.RecheckOrg(e.OrgID)
		}
	}, eventbus.InlineWhenSaturated())
	eventbus.Subscribe(bus, eventbus.Async, name, func(_ context.Context, e events.AccountErased) {
		h.RecheckUser(e.UserID)
		h.RecheckOwner(e.UserID)
	}, eventbus.InlineWhenSaturated())
	eventbus.Subscribe(bus, eventbus.Async, name, func(_ context.Context, e events.AccessRevoked) {
		h.RecheckCredential(e.UserID, e.JTI)
	}, eventbus.InlineWhenSaturated())
}

// RecheckFlow flags the sockets of flowID's room (its sharing changed, or it was deleted) and returns how many.
func (h *Hub) RecheckFlow(flowID string) int {
	if flowID == "" {
		return 0
	}
	h.accessChanges.Add(1)
	return flagRecheck(h.roomTargets(flowID, nil))
}

// RecheckUser flags the sockets userID opened, on any flow (a membership of theirs changed, or their account was
// erased), and returns how many. Not narrowed to the organisation's flows: a user holds at most maxConnsPerUser
// sockets here, so that would save a few lookups at the cost of trusting the organisation recorded at connect.
func (h *Hub) RecheckUser(userID string) int {
	if userID == "" {
		return 0
	}
	return h.recheckWhere(func(c *Client) bool { return c.UserID == userID })
}

// RecheckOrg flags the sockets on orgID's flows (the organisation was deleted and its flows went to their owners)
// and returns how many.
func (h *Hub) RecheckOrg(orgID string) int {
	if orgID == "" {
		return 0
	}
	return h.recheckWhere(func(c *Client) bool { return c.flowOwner.OrgID == orgID })
}

// RecheckOwner flags the sockets on the flows ownerID owns (their account was erased, and the flows with it) and
// returns how many.
func (h *Hub) RecheckOwner(ownerID string) int {
	if ownerID == "" {
		return 0
	}
	return h.recheckWhere(func(c *Client) bool { return c.flowOwner.UserID == ownerID })
}

// RecheckCredential flags the sockets userID opened with the credential jti (it was revoked) and returns how many.
// The user must match too, so an announcement naming another user's credential reaches none of theirs.
func (h *Hub) RecheckCredential(userID, jti string) int {
	if userID == "" || jti == "" {
		return 0
	}
	return h.recheckWhere(func(c *Client) bool { return c.UserID == userID && c.accessJTI == jti })
}

// recheckWhere flags every socket of the hub that match selects: one pass over the rooms under the read lock,
// reading only what each socket fixed before it joined. accessChanges moves first, so a socket joining during the
// pass is either seen by it or sees the change itself (Client.Run).
func (h *Hub) recheckWhere(match func(*Client) bool) int {
	h.accessChanges.Add(1)
	h.mu.RLock()
	var targets []*Client
	for _, r := range h.rooms {
		for c := range r.clients {
			if match(c) {
				targets = append(targets, c)
			}
		}
	}
	h.mu.RUnlock()
	return flagRecheck(targets)
}

func flagRecheck(clients []*Client) int {
	for _, c := range clients {
		c.requestRecheck()
	}
	return len(clients)
}
