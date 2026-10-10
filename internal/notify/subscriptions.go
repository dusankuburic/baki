package notify

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Per-channel event subscriptions (ADR 0021). An organisation's channel receives the event types it chose, the test
// event always; a channel that never chose receives DefaultOrgEvents.

// orgEvents are the event types an organisation's channel can choose, in the order the settings page lists them: what
// the scanner sends to organisation channels (analysis_complete goes to the deployment's channels only).
var orgEvents = []EventType{EventDrift, EventHealthRegression, EventNewRules}

// OrgEvents returns the event types an organisation's channel can subscribe to.
func OrgEvents() []EventType { return slices.Clone(orgEvents) }

// DefaultOrgEvents returns what a channel without a subscription receives: every event an organisation channel
// received before subscriptions existed. It is frozen: an event type added later is one a channel opts into, so no
// saved channel starts receiving something new.
func DefaultOrgEvents() []EventType {
	return []EventType{EventDrift, EventHealthRegression, EventNewRules}
}

// ParseOrgEvents validates a subscription: one or more of OrgEvents, each once, returned in OrgEvents' order.
func ParseOrgEvents(names []string) ([]EventType, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("choose at least one event (pause the channel to receive none)")
	}
	seen := map[EventType]bool{}
	for _, n := range names {
		t := EventType(n)
		if !slices.Contains(orgEvents, t) {
			return nil, fmt.Errorf("unknown event %q (one of %s)", n, joinEvents(orgEvents))
		}
		seen[t] = true
	}
	var out []EventType
	for _, t := range orgEvents {
		if seen[t] {
			out = append(out, t)
		}
	}
	return out, nil
}

// SubscribedEvents is a stored subscription as event types: nil (none chosen) is DefaultOrgEvents.
func SubscribedEvents(stored []string) []EventType {
	if stored == nil {
		return DefaultOrgEvents()
	}
	out := make([]EventType, 0, len(stored))
	for _, s := range stored {
		out = append(out, EventType(s))
	}
	return out
}

func joinEvents(ts []EventType) string {
	s := make([]string, len(ts))
	for i, t := range ts {
		s[i] = string(t)
	}
	return strings.Join(s, ", ")
}

// Subscribed returns n delivering only events (and the test event); the others are dropped without an error.
func Subscribed(n AlertNotifier, events []EventType) AlertNotifier {
	return &subscribed{AlertNotifier: n, events: slices.Clone(events)}
}

type subscribed struct {
	AlertNotifier
	events []EventType
}

// Receives reports whether the channel takes events of type t.
func (s *subscribed) Receives(t EventType) bool {
	return t == EventTest || slices.Contains(s.events, t)
}

func (s *subscribed) Notify(ctx context.Context, ev Event) error {
	if !s.Receives(ev.Type) {
		return nil
	}
	return s.AlertNotifier.Notify(ctx, ev)
}

// Receives reports whether n takes events of type t: false only for a channel subscribed to other events.
func Receives(n AlertNotifier, t EventType) bool {
	if s, ok := n.(interface{ Receives(EventType) bool }); ok {
		return s.Receives(t)
	}
	return true
}
