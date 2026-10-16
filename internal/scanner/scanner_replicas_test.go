package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"pad-core/models"

	"pad-analyzer/internal/notify"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil/redisreplica"
)

// Two replicas sweep one library. The job's lease makes their sweeps take turns; it does not make them one per
// interval: each replica's schedule fires, and whichever finds the lease free sweeps. These tests run the replicas'
// sweeps alternately on one store and one Redis.

// replicaHarness is a skip harness on rep's Redis client, as the app builds the scanner with PAD_MAX_REPLICAS > 1.
func replicaHarness(t *testing.T, b *healthBackend, rep *redisreplica.Replica) *skipHarness {
	t.Helper()
	h := newSkipHarness(t, b)
	WithSharedScans(rep.Client)(h.s)
	return h
}

// TestSharedScans_AFlowOneReplicaScannedIsNotAnalyzedAgainByAnother: a replica sweeping after another analyzes only
// what changed since that sweep, and a replica that joins does not re-analyze the library. With the memory per
// process, B re-analyzed every flow A had scanned, and each change was analyzed once per replica.
func TestSharedScans_AFlowOneReplicaScannedIsNotAnalyzedAgainByAnother(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	for i := range 3 {
		seedFlow(t, b, fmt.Sprintf("f%d", i))
	}
	c := redisreplica.New(t, 2)
	a, other := replicaHarness(t, b, c.Replicas[0]), replicaHarness(t, b, c.Replicas[1])

	a.s.ScanOnce(ctx)
	other.s.ScanOnce(ctx)
	for i := range 3 {
		id := fmt.Sprintf("f%d", i)
		if n := a.runsOf(id); n != 1 {
			t.Fatalf("A analyzed %s %d times, want 1", id, n)
		}
		if n := other.runsOf(id); n != 0 {
			t.Errorf("B analyzed %s %d times after A scanned it unchanged, want 0", id, n)
		}
	}

	// A save bumps f1's version: the next sweep, B's, analyzes it, and A's after it does not.
	if err := b.SaveFlow(ctx, &storageif.FlowDocument{ID: "f1", Name: "f1", Content: json.RawMessage(`{"id":"f1","subflows":[]}`)}); err != nil {
		t.Fatal(err)
	}
	other.s.ScanOnce(ctx)
	a.s.ScanOnce(ctx)
	if n := other.runsOf("f1"); n != 1 {
		t.Errorf("B analyzed the changed f1 %d times, want 1", n)
	}
	if n := a.runsOf("f1"); n != 1 {
		t.Errorf("A analyzed f1 %d times in all, want 1: B had scanned the change", n)
	}
	if n := other.runsOf("f0") + other.runsOf("f2"); n != 0 {
		t.Errorf("B analyzed unchanged flows %d times, want 0", n)
	}
	if ttl := c.Redis.TTL(scanRecordKey.Key("f1")); ttl <= 0 || ttl > scanRecordTTL {
		t.Errorf("the record's TTL is %v, want (0, %v]", ttl, scanRecordTTL)
	}
}

// TestSharedScans_AReplicasSkipKeepsTheNoticeOfTheOther: a flow skipped on another replica's record notes that
// record's "new rules" note again, so the notice it raises is kept from the retention sweep as on one replica.
func TestSharedScans_AReplicasSkipKeepsTheNoticeOfTheOther(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	tenants(t, b, []string{"owner"}, map[string][]string{"org-1": {"owner"}})
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	c := redisreplica.New(t, 2)
	a, other := replicaHarness(t, b, c.Replicas[0]), replicaHarness(t, b, c.Replicas[1])
	other.reports = a.reports
	for _, h := range []*skipHarness{a, other} {
		h.s.SetEventNotifier(&fakeSSE{})
	}
	ids := []string{"n1", "n2"}
	for _, id := range ids {
		seedOwnedFlow(t, b, id, "owner", "org-1")
		setBaselineWith(t, b, id, old)
		a.reports[id] = reportUnder(id, current, finding("hardcoded-url", "b1", models.SeverityWarning))
	}
	notice := storageif.RuleNoticeID("org", "org-1", current.Digest)
	a.s.ScanOnce(ctx)
	if n := allNotices(t, b)[notice]; n == nil || n.FlowCount != len(ids) {
		t.Fatalf("notice after A's sweep = %+v, want one raised by %d flows", n, len(ids))
	}

	b.RuleNotices[notice].UpdatedAt = time.Now().AddDate(0, 0, -40)
	other.s.ScanOnce(ctx)
	if n := other.runsOf("n1") + other.runsOf("n2"); n != 0 {
		t.Fatalf("B analyzed the unchanged flows %d times, want 0", n)
	}
	if res, err := b.PurgeExpiredData(ctx, 30); err != nil || res.RuleNotices != 0 {
		t.Fatalf("retention sweep after B's sweep = %+v, %v; want the notice its skipped flows still raise kept", res, err)
	}
	if n := allNotices(t, b)[notice]; n == nil || n.FlowCount != len(ids) {
		t.Errorf("notice after B's sweep = %+v, want it raised by %d flows", n, len(ids))
	}
}

// TestSharedScans_RedisDownFallsBackToEachReplicasOwnMemory: with Redis gone a sweep still runs, on the replica's
// own memory (a replica that has none re-analyzes, as after a restart); when Redis is back the records are used again.
func TestSharedScans_RedisDownFallsBackToEachReplicasOwnMemory(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	for i := range 2 {
		seedFlow(t, b, fmt.Sprintf("f%d", i))
	}
	c := redisreplica.New(t, 3)
	a, other := replicaHarness(t, b, c.Replicas[0]), replicaHarness(t, b, c.Replicas[1])
	a.s.ScanOnce(ctx)

	c.Kill()
	other.s.ScanOnce(ctx)
	a.s.ScanOnce(ctx)
	if n := other.runsOf("f0") + other.runsOf("f1"); n != 2 {
		t.Errorf("B analyzed %d flows with Redis down and no memory of its own, want 2", n)
	}
	if n := a.runsOf("f0") + a.runsOf("f1"); n != 2 {
		t.Errorf("A analyzed %d times in all with Redis down, want 2: its own memory still skips", n)
	}

	c.Restore(t)
	joined := replicaHarness(t, b, c.Replicas[2])
	joined.s.ScanOnce(ctx)
	if n := joined.runsOf("f0") + joined.runsOf("f1"); n != 0 {
		t.Errorf("a replica that joined after the outage analyzed %d flows, want 0 (the records outlived it)", n)
	}
}

// TestScanner_AnotherReplicaDoesNotDeliverAnAlertAgain proves that the alert de-duplication needs no shared memory:
// the inbox row's id is the alert's (flow, type, signature), its insert is ON CONFLICT DO NOTHING, and only an
// inserted alert is delivered. A replica whose memory has never seen the alert (no Redis here at all) re-analyzes
// the flow and delivers nothing.
func TestScanner_AnotherReplicaDoesNotDeliverAnAlertAgain(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	seedFlow(t, b, "f1")
	if err := b.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: "f1", Keys: []string{"r1:b1"}}); err != nil {
		t.Fatal(err)
	}
	reports := map[string]*models.AnalysisReport{"f1": {FlowID: "f1", Findings: []models.Finding{
		{RuleID: "r1", BlockID: "b1", Severity: models.SeverityWarning},
		{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError},
	}}}
	sweepOn := func() (*capture, *fakeSSE) {
		cap, sse := newCapture(t), &fakeSSE{}
		s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
		s.SetEventNotifier(sse)
		s.ScanOnce(ctx)
		s.Stop()
		return cap, sse
	}
	capA, _ := sweepOn()
	if got := capA.all(); len(got) != 1 || got[0].Type != notify.EventDrift {
		t.Fatalf("A delivered %+v, want the one drift alert", got)
	}
	capB, sseB := sweepOn()
	if got := capB.all(); len(got) != 0 {
		t.Errorf("B delivered %+v again, want nothing: the inbox already holds the alert", got)
	}
	if got := sseB.calls(); len(got) != 0 {
		t.Errorf("B pushed %+v, want nothing", got)
	}
	if alerts, _ := b.ListGovernanceAlerts(ctx, storageif.GovernanceAlertFilter{}); len(alerts) != 1 {
		t.Errorf("inbox holds %d alerts, want 1", len(alerts))
	}
}
