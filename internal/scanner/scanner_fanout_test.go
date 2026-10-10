package scanner

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"

	"pad-core/models"
)

// connCountingCapture is a receiver that counts NEW TCP connections as well as
// requests. The distinction is the whole point of the notifier cache: a
// notifier built per event clones http.DefaultTransport each time, so every
// delivery opens its own connection and none are ever reused.
type connCountingCapture struct {
	srv      *httptest.Server
	requests atomic.Int32
	conns    atomic.Int32
}

func newConnCountingCapture(t *testing.T) *connCountingCapture {
	t.Helper()
	c := &connCountingCapture{}
	c.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	c.srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			c.conns.Add(1)
		}
	}
	c.srv.Start()
	t.Cleanup(c.srv.Close)
	return c
}

// alertingFlows builds n flows in one org, each with findings that trip an
// alert, so one sweep produces n org-channel deliveries.
func alertingFlows(t *testing.T, n int) (*healthBackend, map[string]*models.AnalysisReport) {
	t.Helper()
	b := newBackend()
	reports := map[string]*models.AnalysisReport{}
	for i := 0; i < n; i++ {
		id := "flow-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		// Org-scoped, which is what routes the alert to the org's channels.
		seedOrgFlow(t, b, id, "org-1")
		// A baseline containing only r1:b1 means r2:b2 in the report below is a
		// NEW finding — which is what actually raises an alert. Without the
		// baseline the sweep is silent and a fan-out test asserts nothing.
		if err := b.SetFlowBaseline(context.Background(), &storageif.FlowBaseline{
			FlowID: id, Keys: []string{"r1:b1"},
		}); err != nil {
			t.Fatalf("set baseline: %v", err)
		}
		reports[id] = &models.AnalysisReport{FlowID: id, Findings: []models.Finding{
			{RuleID: "r1", BlockID: "b1", Severity: models.SeverityWarning},
			{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError},
		}}
	}
	return b, reports
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestScanOnce_ReusesOneNotifierPerOrgPerSweep is the connection-churn gate.
//
// dispatchOrgChannels once built a notifier per event per channel, and each
// cloned http.DefaultTransport — so every alert got a fresh connection pool, a
// fresh DNS resolution, and left a transport behind with idle connections
// nothing closes. The channel service's shared client and the per-sweep build
// cache mean keep-alive actually works.
func TestScanOnce_ReusesOneNotifierPerOrgPerSweep(t *testing.T) {
	const flows = 20
	b, reports := alertingFlows(t, flows)
	hook := newConnCountingCapture(t)
	if err := b.SaveOrgChannel(context.Background(), &storageif.OrgChannel{
		ID: "ch1", OrgID: "org-1", Name: "ops", Kind: "webhook", URL: hook.srv.URL, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	global := newCapture(t)
	// Two deliveries at a time: a pooled transport then opens about two connections, a transport per event one per
	// delivery. With the default limit (32) all 20 could overlap and open 20 connections even when pooled, which
	// failed this test under load.
	const limit = 2
	channels := service.NewChannelService(b, service.ChannelOptions{Policy: egress.AllowLoopback,
		Runner: (&lifecycle.Group{}).NewBounded("scanner-fanout-test", limit)})
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, global.srv.URL), 0, channels)
	s.ScanOnce(context.Background())
	s.Stop() // drains in-flight deliveries

	if got := int(hook.requests.Load()); got != flows {
		t.Fatalf("deliveries = %d, want %d", got, flows)
	}
	if conns := int(hook.conns.Load()); conns > 2*limit {
		t.Errorf("opened %d connections for %d deliveries — the notifier (and its transport) "+
			"is being rebuilt per event instead of cached per sweep", conns, flows)
	}
}

// TestScanner_StopWaitsForInFlightDeliveries pins the drain. Deliveries are
// deliberately detached from the sweep context (cancelling mid-POST abandons a
// half-sent alert), so before this they were simply dropped on shutdown.
func TestScanner_StopWaitsForInFlightDeliveries(t *testing.T) {
	b, reports := alertingFlows(t, 1)
	var delivered atomic.Bool
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		delivered.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	if err := b.SaveOrgChannel(context.Background(), &storageif.OrgChannel{
		ID: "ch1", OrgID: "org-1", Name: "ops", Kind: "webhook", URL: slow.URL, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	global := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, global.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	// Let the delivery finish once Stop is already waiting.
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(release)
	}()
	s.Stop()

	if !delivered.Load() {
		t.Error("Stop returned before the in-flight alert delivery completed — shutdown silently drops alerts")
	}
}

// TestScanOnce_BoundsConcurrentDeliveries proves the fan-out cap. A sweep where
// thousands of flows alert used to launch a goroutine, a transport and a DNS
// lookup per alert, all at once.
func TestScanOnce_BoundsConcurrentDeliveries(t *testing.T) {
	const flows = 120
	b, reports := alertingFlows(t, flows)

	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond) // hold the slot so overlap is observable
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer gate.Close()

	if err := b.SaveOrgChannel(context.Background(), &storageif.OrgChannel{
		ID: "ch1", OrgID: "org-1", Name: "ops", Kind: "webhook", URL: gate.URL, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	global := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, global.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(context.Background())
	s.Stop()

	if got := peak.Load(); got > service.ChannelDeliveryLimit {
		t.Errorf("peak concurrent deliveries = %d, cap is %d", got, service.ChannelDeliveryLimit)
	}
	if !waitFor(func() bool { return inFlight.Load() == 0 }, 2*time.Second) {
		t.Error("deliveries still in flight after Stop")
	}
}
