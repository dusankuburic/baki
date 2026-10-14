package websocket

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/txhook"
)

// joinWith puts a connection of userID on flowID into h's room, as the handler admits it: its flow's owner and the
// credential it follows fixed before the join.
func joinWith(t *testing.T, h *Hub, flowID, userID string, owner FlowOwner, jti string) *Client {
	t.Helper()
	c := &Client{UserID: userID, DisplayName: userID, hub: h, flowID: flowID, connID: flowID + "/" + userID + "/" + jti,
		send: make(chan Envelope, sendBufferCap), recheckNow: make(chan struct{}, 1), flowOwner: owner, accessJTI: jti}
	h.Join(flowID, c)
	return c
}

// flagged reports whether c was asked for a recheck since the last call, and clears the request.
func flagged(c *Client) bool {
	select {
	case <-c.recheckNow:
		return true
	default:
		return false
	}
}

// TestRecheck_EachChangeReachesItsSockets: every access change flags exactly the sockets it can reach, from what the
// hub holds (rooms, users, the flow's owner and organisation, the credential); a change that only widens access, or
// touches content, flags none.
func TestRecheck_EachChangeReachesItsSockets(t *testing.T) {
	h := NewHub()
	defer h.Close()
	bus := eventbus.New(nil, time.Second)
	SubscribeAccessRechecks(bus, h)
	sockets := map[string]*Client{
		"alice@f1": joinWith(t, h, "f1", "alice", FlowOwner{UserID: "alice", OrgID: "o1"}, "j-alice"),
		"bob@f1":   joinWith(t, h, "f1", "bob", FlowOwner{UserID: "alice", OrgID: "o1"}, "j-bob"),
		"bob@f2":   joinWith(t, h, "f2", "bob", FlowOwner{UserID: "carol"}, "j-bob"),
		"carol@f3": joinWith(t, h, "f3", "carol", FlowOwner{UserID: "carol", OrgID: "o2"}, "pat:t1"),
	}

	for _, tc := range []struct {
		name  string
		event eventbus.Event
		want  []string
	}{
		{"collaborators changed", events.FlowChanged{FlowID: "f1", Kind: events.FlowSharingChanged}, []string{"alice@f1", "bob@f1"}},
		{"flow deleted", events.FlowChanged{FlowID: "f1", Kind: events.FlowDeleted}, []string{"alice@f1", "bob@f1"}},
		{"content saved", events.FlowChanged{FlowID: "f1", Kind: events.FlowUpdated}, nil},
		{"flow created", events.FlowChanged{FlowID: "f9", Kind: events.FlowCreated}, nil},
		{"member removed", events.OrgMemberChanged{OrgID: "o1", UserID: "bob", Kind: events.OrgMemberRemoved}, []string{"bob@f1", "bob@f2"}},
		{"member's role changed", events.OrgMemberChanged{OrgID: "o1", UserID: "bob", Role: "viewer", Kind: events.OrgMemberRoleChanged}, []string{"bob@f1", "bob@f2"}},
		{"member added", events.OrgMemberChanged{OrgID: "o1", UserID: "bob", Kind: events.OrgMemberAdded}, nil},
		{"organisation deleted", events.OrgMemberChanged{OrgID: "o1", Kind: events.OrgDeleted}, []string{"alice@f1", "bob@f1"}},
		{"account erased", events.AccountErased{UserID: "carol"}, []string{"bob@f2", "carol@f3"}},
		{"token revoked", events.AccessRevoked{UserID: "carol", JTI: "pat:t1"}, []string{"carol@f3"}},
		{"session logged out", events.AccessRevoked{UserID: "bob", JTI: "j-bob"}, []string{"bob@f1", "bob@f2"}},
		{"another user's credential named", events.AccessRevoked{UserID: "alice", JTI: "j-bob"}, nil},
		{"no credential named", events.AccessRevoked{UserID: "bob"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus.Publish(context.Background(), tc.event)
			want := map[string]bool{}
			for _, s := range tc.want {
				want[s] = true
			}
			for name, c := range sockets {
				if got := flagged(c); got != want[name] {
					t.Errorf("%s flagged = %v, want %v", name, got, want[name])
				}
			}
		})
	}
}

// TestRecheck_OnlyAfterCommit: a change flags its sockets once its transaction has committed, never before (the
// recheck would read the old grants and keep the socket) and never for a rolled-back change.
func TestRecheck_OnlyAfterCommit(t *testing.T) {
	h := NewHub()
	defer h.Close()
	bus := eventbus.New(nil, time.Second)
	SubscribeAccessRechecks(bus, h)
	c := joinWith(t, h, "f1", "bob", FlowOwner{UserID: "alice"}, "j-bob")

	ctx, reg := txhook.With(context.Background())
	bus.Publish(ctx, events.FlowChanged{FlowID: "f1", Kind: events.FlowSharingChanged})
	if flagged(c) {
		t.Fatal("socket flagged before the commit")
	}
	reg.Abort()
	if flagged(c) {
		t.Fatal("a rolled-back change flagged the socket")
	}

	ctx, reg = txhook.With(context.Background())
	bus.Publish(ctx, events.FlowChanged{FlowID: "f1", Kind: events.FlowSharingChanged})
	reg.Commit(nil)
	if !flagged(c) {
		t.Fatal("socket not flagged after the commit")
	}
}

// gatedChecker counts recheck passes (CanEdit runs first on every pass) and how many run at once; while gate is
// set, each pass waits for it. Users in denied lose access. connectChangesFlow, when set, runs once, inside the
// next CheckAccess after it has read denied: a change committing just after the connect check read the grants.
type gatedChecker struct {
	owner              FlowOwner
	passes             atomic.Int32
	inFlight, maxSeen  atomic.Int32
	mu                 sync.Mutex
	gate               chan struct{}
	denied             map[string]bool
	connectChangesFlow func(flowID string)
}

func (g *gatedChecker) CheckAccess(_ context.Context, flowID string, actor *auth.Actor) (FlowOwner, error) {
	g.mu.Lock()
	denied, change := g.denied[actor.UserID], g.connectChangesFlow
	g.connectChangesFlow = nil
	g.mu.Unlock()
	if change != nil {
		change(flowID)
	}
	if denied {
		return FlowOwner{}, ErrAccessDenied
	}
	return g.owner, nil
}

func (g *gatedChecker) CanEdit(ctx context.Context, _ string, actor *auth.Actor) bool {
	g.passes.Add(1)
	n := g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	for {
		seen := g.maxSeen.Load()
		if n <= seen || g.maxSeen.CompareAndSwap(seen, n) {
			break
		}
	}
	g.mu.Lock()
	gate, denied := g.gate, g.denied[actor.UserID]
	g.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	return !denied
}

func (g *gatedChecker) set(fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fn()
}

// connect opens a socket of userID on flowID through the handler, checked by checker, and reads it up to its
// presence.sync.
func connect(t *testing.T, h *Hub, checker FlowAccessChecker, userID, flowID string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(Handler(h, testIdentity(userID, userID), nil, checker, nil))
	t.Cleanup(srv.Close)
	conn := dial(t, srv, flowID)
	t.Cleanup(func() { conn.Close() })
	_ = readEnvelope(t, conn) // own presence.join
	_ = readSync(t, conn)
	return conn
}

// closedWithin reports whether the server closes conn within d.
func closedWithin(conn *websocket.Conn, d time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			var ne net.Error
			return !errors.As(err, &ne) || !ne.Timeout()
		}
	}
}

// TestRecheck_BurstCostsOnePass: a burst of changes reaching one socket while it is re-checking leaves one more
// pass pending, not one per change.
func TestRecheck_BurstCostsOnePass(t *testing.T) {
	h := NewHub()
	defer h.Close()
	checker := &gatedChecker{}
	connect(t, h, checker, "bob", "f1")
	gate := make(chan struct{})
	checker.set(func() { checker.gate = gate })
	checker.passes.Store(0)

	h.RecheckFlow("f1")
	if !eventually(t, 2*time.Second, func() bool { return checker.inFlight.Load() == 1 }) {
		t.Fatal("the first change started no recheck")
	}
	for range 100 {
		h.RecheckFlow("f1")
	}
	checker.set(func() { checker.gate = nil })
	close(gate)

	if !eventually(t, 2*time.Second, func() bool { return checker.passes.Load() == 2 }) {
		t.Fatalf("%d recheck passes after a burst of 101 changes, want 2", checker.passes.Load())
	}
	time.Sleep(100 * time.Millisecond)
	if n := checker.passes.Load(); n != 2 {
		t.Fatalf("%d recheck passes after a burst of 101 changes, want 2", n)
	}
}

// TestRecheck_ConcurrencyIsBounded: a change reaching many sockets at once (an organisation deleted) runs at most
// maxConcurrentRechecks passes at a time, and every socket is re-checked in the end.
func TestRecheck_ConcurrencyIsBounded(t *testing.T) {
	h := NewHub()
	defer h.Close()
	checker := &gatedChecker{owner: FlowOwner{UserID: "owner", OrgID: "o1"}}
	const sockets = 3 * maxConcurrentRechecks
	for i := range sockets {
		connect(t, h, checker, "user-"+strconv.Itoa(i), "f"+strconv.Itoa(i%3))
	}
	gate := make(chan struct{})
	checker.set(func() { checker.gate = gate })
	checker.passes.Store(0)
	checker.maxSeen.Store(0)

	if n := h.RecheckOrg("o1"); n != sockets {
		t.Fatalf("RecheckOrg flagged %d sockets, want %d", n, sockets)
	}
	if !eventually(t, 2*time.Second, func() bool { return checker.inFlight.Load() == maxConcurrentRechecks }) {
		t.Fatalf("%d rechecks in flight, want %d", checker.inFlight.Load(), maxConcurrentRechecks)
	}
	time.Sleep(100 * time.Millisecond)
	if n := checker.maxSeen.Load(); n != maxConcurrentRechecks {
		t.Fatalf("%d rechecks ran at once, want at most %d", n, maxConcurrentRechecks)
	}
	checker.set(func() { checker.gate = nil })
	close(gate)
	if !eventually(t, 2*time.Second, func() bool { return checker.passes.Load() == sockets }) {
		t.Fatalf("%d of %d sockets re-checked", checker.passes.Load(), sockets)
	}
}

// TestRecheck_PeriodicPassIsNotHeldBack: a flagged socket waiting for a recheck slot still gets its periodic pass,
// so the backstop holds when the slots are all taken.
func TestRecheck_PeriodicPassIsNotHeldBack(t *testing.T) {
	h := NewHub()
	defer h.Close()
	h.reauthzPeriod = 300 * time.Millisecond
	checker := &gatedChecker{denied: map[string]bool{}}
	conn := connect(t, h, checker, "bob", "f1")
	for range maxConcurrentRechecks {
		h.recheckSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range maxConcurrentRechecks {
			<-h.recheckSlots
		}
	})

	checker.set(func() { checker.denied["bob"] = true })
	h.RecheckFlow("f1")
	if !closedWithin(conn, 2*time.Second) {
		t.Fatal("a socket waiting for a recheck slot missed its periodic pass and kept its access")
	}
}

// TestRecheck_ChangeDuringAdmissionIsCaughtUp: a change that commits after the connect check read the old grants,
// but before the socket joined its room, finds no socket to flag; the socket re-checks once it has joined.
func TestRecheck_ChangeDuringAdmissionIsCaughtUp(t *testing.T) {
	h := NewHub()
	defer h.Close()
	checker := &gatedChecker{denied: map[string]bool{}}
	checker.connectChangesFlow = func(flowID string) {
		checker.set(func() { checker.denied["bob"] = true })
		if n := h.RecheckFlow(flowID); n != 0 {
			t.Errorf("the change flagged %d sockets before the socket joined", n)
		}
	}
	srv := httptest.NewServer(Handler(h, testIdentity("bob", "bob"), nil, checker, nil))
	defer srv.Close()
	conn := dial(t, srv, "f1")
	defer conn.Close()
	if !closedWithin(conn, 2*time.Second) {
		t.Fatal("a socket admitted while its access was removed kept it until the periodic pass")
	}
}

// TestRecheck_ReplicatesWhatEachChangeFlags: the nudge the other replicas get names the same target the local
// recheck flags, and none is sent for a change that only widens access or touches content.
func TestRecheck_ReplicatesWhatEachChangeFlags(t *testing.T) {
	h := NewHub()
	defer h.Close()
	bus := eventbus.New(nil, time.Second)
	rec := &eventbus.ReplicasRecorder{}
	bus.SetReplicas(rec)
	SubscribeAccessRechecks(bus, h)
	for _, tc := range []struct {
		name  string
		event eventbus.Event
		want  []string
	}{
		{"sharing", events.FlowChanged{FlowID: "f1", Kind: events.FlowSharingChanged}, []string{"recheck.ws-access{id=f1,what=flow}"}},
		{"flow deleted", events.FlowChanged{FlowID: "f1", Kind: events.FlowDeleted}, []string{"recheck.ws-access{id=f1,what=flow}"}},
		{"content saved", events.FlowChanged{FlowID: "f1", Kind: events.FlowUpdated}, nil},
		{"member removed", events.OrgMemberChanged{OrgID: "o1", UserID: "bob", Kind: events.OrgMemberRemoved}, []string{"recheck.ws-access{id=bob,what=user}"}},
		{"role changed", events.OrgMemberChanged{OrgID: "o1", UserID: "bob", Kind: events.OrgMemberRoleChanged}, []string{"recheck.ws-access{id=bob,what=user}"}},
		{"member added", events.OrgMemberChanged{OrgID: "o1", UserID: "bob", Kind: events.OrgMemberAdded}, nil},
		{"org deleted", events.OrgMemberChanged{OrgID: "o1", Kind: events.OrgDeleted}, []string{"recheck.ws-access{id=o1,what=org}"}},
		{"account erased", events.AccountErased{UserID: "carol"}, []string{"recheck.ws-access{id=carol,what=user}", "recheck.ws-access{id=carol,what=owner}"}},
		{"credential revoked", events.AccessRevoked{UserID: "bob", JTI: "j-bob"}, []string{"recheck.ws-access{jti=j-bob,user=bob,what=credential}"}},
	} {
		rec.Reset()
		bus.Publish(context.Background(), tc.event)
		if got := rec.Published(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: published %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A peer's nudge (recheck.ws-access) flags this replica's sockets exactly as the local change would; the peer did
// its own. An empty scope is the receiver's resync after a gap: it may have missed any change, so every socket
// re-checks. A scope that names nothing it knows flags nothing.
func TestRecheck_AppliesAPeersNudge(t *testing.T) {
	h := NewHub()
	defer h.Close()
	bus := eventbus.New(nil, time.Second)
	SubscribeAccessRechecks(bus, h)
	sockets := map[string]*Client{
		"alice@f1": joinWith(t, h, "f1", "alice", FlowOwner{UserID: "alice", OrgID: "o1"}, "j-alice"),
		"bob@f1":   joinWith(t, h, "f1", "bob", FlowOwner{UserID: "alice", OrgID: "o1"}, "j-bob"),
		"bob@f2":   joinWith(t, h, "f2", "bob", FlowOwner{UserID: "carol"}, "j-bob"),
		"carol@f3": joinWith(t, h, "f3", "carol", FlowOwner{UserID: "carol", OrgID: "o2"}, "pat:t1"),
	}
	for _, tc := range []struct {
		name  string
		scope eventbus.Scope
		want  []string
	}{
		{"flow", eventbus.NewScope("what", "flow", "id", "f1"), []string{"alice@f1", "bob@f1"}},
		{"user", eventbus.NewScope("what", "user", "id", "bob"), []string{"bob@f1", "bob@f2"}},
		{"org", eventbus.NewScope("what", "org", "id", "o1"), []string{"alice@f1", "bob@f1"}},
		{"owner", eventbus.NewScope("what", "owner", "id", "carol"), []string{"bob@f2", "carol@f3"}},
		{"credential", eventbus.NewScope("what", "credential", "user", "carol", "jti", "pat:t1"), []string{"carol@f3"}},
		{"another user's credential: the user must match", eventbus.NewScope("what", "credential", "user", "alice", "jti", "j-bob"), nil},
		{"a credential without its user", eventbus.NewScope("what", "credential", "jti", "j-bob"), nil},
		{"unknown kind", eventbus.NewScope("what", "galaxy", "id", "f1"), nil},
		{"no id", eventbus.NewScope("what", "flow"), nil},
		{"resync: every socket", nil, []string{"alice@f1", "bob@f1", "bob@f2", "carol@f3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !bus.DeliverRemote("recheck.ws-access", tc.scope) {
				t.Fatal("no applier for recheck.ws-access")
			}
			want := map[string]bool{}
			for _, s := range tc.want {
				want[s] = true
			}
			for name, c := range sockets {
				if got := flagged(c); got != want[name] {
					t.Errorf("%s flagged = %v, want %v", name, got, want[name])
				}
			}
		})
	}
}
