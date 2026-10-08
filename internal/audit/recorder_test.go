package audit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/reqinfo"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// waitFor polls cond for up to 2 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNewEvent_ReadsActorAndRequestInfo: who acted comes from the context's actor (a session, an API token, a
// background job), where from from the request's reqinfo.Info; the entry is copied as given.
func TestNewEvent_ReadsActorAndRequestInfo(t *testing.T) {
	entry := Entry{Action: ActionFlowSave, ResourceType: "flow", ResourceID: "f-1", OrgID: "org-1", Meta: map[string]string{"k": "v"}}
	inRequest := func(ctx context.Context) context.Context {
		return reqinfo.With(ctx, &reqinfo.Info{ClientIP: "203.0.113.7", PeerIP: "10.0.0.1", UserAgent: "Mozilla/5.0"})
	}
	bg := context.Background()
	cases := []struct {
		name                                  string
		ctx                                   context.Context
		user, email, kind, token, job, ip, ua string
	}{
		{"session", inRequest(auth.WithClaims(bg, &auth.Claims{UserID: "u-1", Email: "a@example.com", Role: auth.RoleMember})),
			"u-1", "a@example.com", "user", "", "", "203.0.113.7", "Mozilla/5.0"},
		{"api token", inRequest(auth.WithClaims(bg, auth.ClaimsForPAT("u-1", "a@example.com", auth.RoleMember, "tok-1", nil, []string{"read"}))),
			"u-1", "a@example.com", "token", "tok-1", "", "203.0.113.7", "Mozilla/5.0"},
		{"background job", auth.WithActor(bg, auth.SystemActor("retention")),
			"", "", "system", "", "retention", "", ""},
		// Local mode has no claims: its rows always had an empty user.
		{"local user", inRequest(auth.WithActor(bg, auth.LocalActor("local"))),
			"", "", "user", "", "", "203.0.113.7", "Mozilla/5.0"},
		{"nobody outside a request", bg, "", "", "", "", "", "", ""},
		// Recorded after the account was erased in the same request: the action stays, the identity does not.
		{"erased actor", WithErasedActor(inRequest(auth.WithClaims(bg, auth.ClaimsForPAT("u-1", "a@example.com", auth.RoleMember, "tok-1", nil, []string{"read"})))),
			"", "", "token", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := NewEvent(tc.ctx, entry)
			if ev.UserID != tc.user || ev.Email != tc.email || ev.ActorKind != tc.kind || ev.TokenID != tc.token ||
				ev.ActorJob != tc.job || ev.IP != tc.ip || ev.UserAgent != tc.ua {
				t.Errorf("event = %+v", ev)
			}
			if ev.Action != entry.Action || ev.ResourceType != "flow" || ev.ResourceID != "f-1" || ev.OrgID != "org-1" ||
				ev.Meta["k"] != "v" || ev.ID == "" || ev.CreatedAt.IsZero() || ev.CreatedAt.Location() != time.UTC {
				t.Errorf("entry not carried over: %+v", ev)
			}
		})
	}
}

// Desktop has no server-side store: a recorder without one, and a nil recorder, record nothing and start and
// stop as no-ops.
func TestRecorder_WithoutStoreRecordsNothing(t *testing.T) {
	var nilRecorder *Recorder
	nilRecorder.Record(context.Background(), Entry{Action: ActionLogin})
	nilRecorder.Start()
	nilRecorder.Stop()

	r := New(nil, Config{})
	r.Start()
	r.Record(context.Background(), Entry{Action: ActionLogin})
	r.Stop()
	if r.started.Load() {
		t.Error("a recorder without a store started a pool")
	}
}

// Before Start (and for a recorder never started) each event is a detached write of its own.
func TestRecord_BeforeStartWritesDirectly(t *testing.T) {
	w := &fakeWriter{}
	r := New(w, Config{})
	r.Record(auth.WithActor(context.Background(), auth.SystemActor("scanner")), Entry{Action: ActionScanNow, ResourceType: "scanner"})
	waitFor(t, "the detached write", func() bool { return len(w.savedEvents()) == 1 })
	if got := w.savedEvents()[0]; got.ActorKind != "system" || got.ActorJob != "scanner" {
		t.Errorf("saved %+v, want the system actor", got)
	}
}

// batchWriter is a store with the batch capability, as Postgres has.
type batchWriter struct {
	fakeWriter
	sink fakeBatchSink
}

func (b *batchWriter) SaveAuditEvents(ctx context.Context, events []*storageif.AuditEvent) error {
	return b.sink.SaveAuditEvents(ctx, events)
}

// TestRecorder_StopDrainsTheQueue: every event recorded before Stop is written by the time Stop returns, in
// batches; one recorded afterwards goes to the log fallback.
func TestRecorder_StopDrainsTheQueue(t *testing.T) {
	w := &batchWriter{}
	r := New(w, Config{})
	r.Start()
	const n = 200
	for i := range n {
		r.Record(context.Background(), Entry{Action: ActionFlowSave, ResourceID: fmt.Sprint(i)})
	}
	r.Stop()
	w.sink.mu.Lock()
	written := len(w.sink.batch)
	w.sink.mu.Unlock()
	if written+w.count() != n {
		t.Fatalf("written %d batched + %d single, want %d", written, w.count(), n)
	}
	if calls := int(w.sink.calls.Load()); calls >= n {
		t.Errorf("%d batch writes for %d events: not batched", calls, n)
	}

	before := metrics.AuditDroppedCount("closed")
	r.Record(context.Background(), Entry{Action: ActionFlowSave})
	if got := metrics.AuditDroppedCount("closed") - before; got != 1 {
		t.Errorf("closed drops after Stop = %v, want 1", got)
	}
	r.Stop() // idempotent
}

func TestWaitWithBudget(t *testing.T) {
	var done sync.WaitGroup
	if !waitWithBudget(&done, time.Second) {
		t.Error("a completed WaitGroup must return true immediately")
	}
	var stuck sync.WaitGroup
	stuck.Add(1)
	defer stuck.Done()
	start := time.Now()
	if waitWithBudget(&stuck, 50*time.Millisecond) {
		t.Error("a stuck WaitGroup must return false")
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("waited %v for a 50ms budget", el)
	}
}

// TestStop_BoundedWhenWorkerStuck: Stop used to wait for the workers with no deadline. Simulate a worker that
// never finishes (a flush against an unreachable database) and assert Stop returns within the budget instead of
// hanging until fx's StopTimeout SIGKILLs the process.
func TestStop_BoundedWhenWorkerStuck(t *testing.T) {
	r := &Recorder{queue: make(chan *storageif.AuditEvent), shutdownBudget: 100 * time.Millisecond}
	r.started.Store(true)
	r.workers.Add(1) // the stuck worker
	defer r.workers.Done()

	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung on a stuck worker; it must give up after the shutdown budget")
	}
}

// TestEnqueue_RecoversSendOnClosedChannel proves the recover guard catches the TOCTOU window: Stop sets closed
// and closes the queue as two separate statements, so a sender past the flag check reaches the send AFTER the
// close. Reproduced deterministically by closing the queue WITHOUT setting the flag.
func TestEnqueue_RecoversSendOnClosedChannel(t *testing.T) {
	r := &Recorder{queue: make(chan *storageif.AuditEvent, 1)}
	close(r.queue)
	if sent, reason := r.enqueue(ev("a")); sent || reason != "closed" {
		t.Fatalf("got (sent=%v, reason=%q), want (false, \"closed\")", sent, reason)
	}
}

// TestEnqueue_DivertsWhenFull: a full queue returns "full" and a recorder closed via the flag returns "closed",
// so callers route to the spill queue or the fallback sink instead of dropping silently.
func TestEnqueue_DivertsWhenFull(t *testing.T) {
	r := &Recorder{queue: make(chan *storageif.AuditEvent, 1)}
	r.queue <- ev("a")
	if sent, reason := r.enqueue(ev("b")); sent || reason != "full" {
		t.Fatalf("full queue: got (sent=%v, reason=%q), want (false, \"full\")", sent, reason)
	}
	r.closed.Store(true)
	if sent, reason := r.enqueue(ev("c")); sent || reason != "closed" {
		t.Fatalf("closed recorder: got (sent=%v, reason=%q), want (false, \"closed\")", sent, reason)
	}
}

// An event that finds the queue full spills to disk, and is replayed from there.
func TestRecord_FullQueueSpills(t *testing.T) {
	r := NewCapture(make(chan *storageif.AuditEvent, 1))
	r.spill = newTestSpill(t, 0)
	r.Record(context.Background(), Entry{Action: "first"})
	r.Record(context.Background(), Entry{Action: "second"})
	if got := r.spill.Drain(10); len(got) != 1 || got[0].Action != "second" {
		t.Fatalf("spilled %v, want the second event", got)
	}
}

// TestNewWithSink_CapturesWhileTheSinkNamesAChannel: a NewWithSink recorder puts each row, in order, on the
// channel its sink names, and a full channel diverts the row to the log fallback; while the sink names none,
// the row is a detached write to the store. Without a store it records nothing.
func TestNewWithSink_CapturesWhileTheSinkNamesAChannel(t *testing.T) {
	store := &fakeWriter{}
	var current atomic.Pointer[chan *storageif.AuditEvent]
	sink := func() chan<- *storageif.AuditEvent {
		if p := current.Load(); p != nil {
			return *p
		}
		return nil
	}
	r := NewWithSink(store, sink)

	ch := make(chan *storageif.AuditEvent, 2)
	current.Store(&ch)
	r.Record(context.Background(), Entry{Action: "first"})
	r.Record(context.Background(), Entry{Action: "second"})
	before := metrics.AuditDroppedCount("full")
	r.Record(context.Background(), Entry{Action: "third"})
	if got := metrics.AuditDroppedCount("full") - before; got != 1 {
		t.Errorf("full drops = %v, want 1", got)
	}
	if a, b := <-ch, <-ch; a.Action != "first" || b.Action != "second" {
		t.Errorf("captured %q, %q; want first, second", a.Action, b.Action)
	}
	if store.count() != 0 {
		t.Error("the store was written while the sink named a channel")
	}

	current.Store(nil)
	r.Record(context.Background(), Entry{Action: "direct"})
	waitFor(t, "the direct write", func() bool { return store.count() == 1 })

	current.Store(&ch)
	NewWithSink(nil, sink).Record(context.Background(), Entry{Action: "nowhere"})
	if len(ch) != 0 {
		t.Error("a recorder without a store captured a row")
	}
}

// TestProvide_Lifecycle: fx starts the pool, and stopping the app drains what was recorded. A desktop app (no
// store) gets a recorder that records nothing.
func TestProvide_Lifecycle(t *testing.T) {
	cfg := &config.Config{}
	cfg.Governance.AuditSpillDir = t.TempDir()

	store := testutil.NewFakeBackend()
	lc := fxtest.NewLifecycle(t)
	r := Provide(lc, cfg, store)
	if r.spill == nil {
		t.Error("the configured spill directory was not used")
	}
	lc.RequireStart()
	if !r.started.Load() {
		t.Fatal("the app started but the recorder's pool did not")
	}
	r.Record(auth.WithActor(context.Background(), auth.SystemActor("scanner")), Entry{Action: ActionScanNow, OrgID: "org-1"})
	lc.RequireStop()
	saved := store.SavedAuditEvents()
	if len(saved) != 1 || saved[0].ActorJob != "scanner" || saved[0].OrgID != "org-1" {
		t.Fatalf("saved %+v, want the scanner's row", saved)
	}

	cfg.Governance.AuditSpillDir = "off"
	desktop := fxtest.NewLifecycle(t)
	d := Provide(desktop, cfg, nil)
	desktop.RequireStart()
	d.Record(context.Background(), Entry{Action: ActionLogin})
	desktop.RequireStop()
	if d.spill != nil || d.queue != nil {
		t.Error("spill \"off\" or no store still built a spill queue or a pool")
	}
}
