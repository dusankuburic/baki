package testutil

import (
	"context"
	"sync"
)

// Audiences a CountingNotifier records (Emitted.Audience).
const (
	AudienceUser = "user"
	AudienceOrg  = "org"
	AudienceAll  = "all"
)

// CountingNotifier implements service.EventNotifier for tests: it counts every emit and records its
// audience, name and payload.
type CountingNotifier struct {
	mu           sync.Mutex
	count        int
	events       []Emitted
	noSubscriber bool
}

// Emitted is one recorded emit: Audience is AudienceUser, AudienceOrg or AudienceAll, Target the user or
// organisation id ("" for AudienceAll).
type Emitted struct {
	Audience string
	Target   string
	Name     string
	Data     any
}

func (n *CountingNotifier) record(e Emitted) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.count++
	n.events = append(n.events, e)
}

func (n *CountingNotifier) EmitTo(userID, name string, data any) {
	n.record(Emitted{Audience: AudienceUser, Target: userID, Name: name, Data: data})
}

func (n *CountingNotifier) EmitToOrg(_ context.Context, orgID, name string, data any) {
	n.record(Emitted{Audience: AudienceOrg, Target: orgID, Name: name, Data: data})
}

func (n *CountingNotifier) EmitToAll(name string, data any) {
	n.record(Emitted{Audience: AudienceAll, Name: name, Data: data})
}

// HasSubscriber reports a connected client unless SetNoSubscriber(true) was
// called; tests use that to drive subscriber-liveness paths (e.g. the chat
// stream watchdog).
func (n *CountingNotifier) HasSubscriber(userID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return !n.noSubscriber
}

func (n *CountingNotifier) SetNoSubscriber(v bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.noSubscriber = v
}

func (n *CountingNotifier) Count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.count
}

func (n *CountingNotifier) Events() []Emitted {
	n.mu.Lock()
	defer n.mu.Unlock()
	cp := make([]Emitted, len(n.events))
	copy(cp, n.events)
	return cp
}

func (n *CountingNotifier) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.count = 0
	n.events = nil
}
