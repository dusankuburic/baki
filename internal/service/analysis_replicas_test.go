package service

import (
	"context"
	"testing"
	"time"

	"pad-core/models"

	"pad-analyzer/internal/testutil/redisreplica"
)

// Two replicas analyze one stored flow behind a balancer that sends each request anywhere. Each has its own analyzer
// (its own report cache) and its own pair and claims; they share a Redis, as with PAD_MAX_REPLICAS > 1.

const replicaFlow = "flow-shared"

// replicaAnalysis is one replica's analysis service on rep's Redis client.
func replicaAnalysis(t *testing.T, rep *redisreplica.Replica) *AnalysisService {
	t.Helper()
	svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil, AnalysisOptions{Shared: rep.Client})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// storedDoc is the stored flow as a replica resolves it for a request: a document of its own with the flow's id. The
// version names the content (a different block name is different content).
func storedDoc(version string) *models.FlowDocument {
	return makeAnalyzableDoc(replicaFlow,
		models.Block{ID: "b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show " + version, SubflowID: "main", LineNumber: 1},
		models.Block{ID: "b2", Type: models.BlockTypeAction, RawType: "Variables.SetVariable", Name: "Set", SubflowID: "main", LineNumber: 2},
	)
}

func analyzeFresh(t *testing.T, svc *AnalysisService, doc *models.FlowDocument) (*models.AnalysisReport, bool) {
	t.Helper()
	r, fresh, err := svc.AnalyzeFlowFresh(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	return r, fresh
}

// TestSharedAnalysis_ARunRecordedOnOneReplicaIsNotFreshOnAnother: the dashboard record (a trend row) and the analysis
// webhook run once per run of the flow, wherever it is analyzed; a newer run is fresh wherever it lands, content that
// changes back is a new run, and a claim given back (a dropped record) is fresh again on any replica.
func TestSharedAnalysis_ARunRecordedOnOneReplicaIsNotFreshOnAnother(t *testing.T) {
	c := redisreplica.New(t, 2)
	a, b := replicaAnalysis(t, c.Replicas[0]), replicaAnalysis(t, c.Replicas[1])

	if _, fresh := analyzeFresh(t, a, storedDoc("v1")); !fresh {
		t.Fatal("the first run on A is not fresh")
	}
	rb, fresh := analyzeFresh(t, b, storedDoc("v1"))
	if fresh {
		t.Error("the same run on B is fresh again: it would record a second trend row and send a second webhook")
	}
	if _, fresh := analyzeFresh(t, b, storedDoc("v2")); !fresh {
		t.Error("a new run on B is not fresh")
	}
	if _, fresh := analyzeFresh(t, a, storedDoc("v2")); fresh {
		t.Error("B's new run, analyzed again on A, is fresh")
	}
	r1, fresh := analyzeFresh(t, a, storedDoc("v1"))
	if !fresh {
		t.Error("content changed back to v1 is a new run, and not fresh")
	}
	// A record that could not start gives its claim back: the next analysis records it, on either replica.
	b.ReleaseRecord(storedDoc("v1"), rb)
	if _, fresh := analyzeFresh(t, b, storedDoc("v1")); !fresh {
		t.Error("after the claim was given back the run is not fresh on B")
	}
	a.ReleaseRecord(storedDoc("v1"), r1)
	if _, fresh := analyzeFresh(t, a, storedDoc("v1")); !fresh {
		t.Error("after the claim was given back the run is not fresh on A")
	}
	if ttl := c.Redis.TTL(analysisClaimKey.Key("path", replicaFlow)); ttl <= 0 || ttl > analysisClaimTTL {
		t.Errorf("the claim's TTL is %v, want (0, %v]", ttl, analysisClaimTTL)
	}
}

// TestSharedAnalysis_AnotherReplicaServesTheCurrentRunAndDiffsAgainstThePrevious: the export (CurrentReport) and
// the diff (AnalyzeFlow, then PreviousReport) on a replica that did not analyze the runs answer with the runs the
// deployment made; before, the export answered "run analysis first" and the diff had no previous run.
func TestSharedAnalysis_AnotherReplicaServesTheCurrentRunAndDiffsAgainstThePrevious(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 2)
	a, b := replicaAnalysis(t, c.Replicas[0]), replicaAnalysis(t, c.Replicas[1])
	r1, err := a.AnalyzeFlow(ctx, storedDoc("v1"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.AnalyzeFlow(ctx, storedDoc("v2"))
	if err != nil {
		t.Fatal(err)
	}

	cur, ok := b.CurrentReport(storedDoc("v2"))
	if !ok || cur == nil || cur.ContentHash != r2.ContentHash || len(cur.Findings) != len(r2.Findings) {
		t.Fatalf("B's current report = (content %q, %v), want A's latest run %q", contentOf(cur), ok, r2.ContentHash)
	}
	if cur.RuleSet == nil || cur.RuleSet.Digest != r2.RuleSet.Digest {
		t.Errorf("B's current report lost its rule set: %+v", cur.RuleSet)
	}
	if _, err := b.AnalyzeFlow(ctx, storedDoc("v2")); err != nil {
		t.Fatal(err)
	}
	prev, has := b.PreviousReport(storedDoc("v2"))
	if !has || prev == nil || prev.ContentHash != r1.ContentHash {
		t.Fatalf("B's previous run = (content %q, %v), want A's first run %q", contentOf(prev), has, r1.ContentHash)
	}
	// A's own pair holds both runs: it answers from memory, with its own reports.
	if p, has := a.PreviousReport(storedDoc("v2")); !has || p != r1 {
		t.Errorf("A's previous run = (%p, %v), want its own report %p", p, has, r1)
	}
	if cur, ok := a.CurrentReport(storedDoc("v2")); !ok || cur != r2 {
		t.Errorf("A's current run = (%p, %v), want its own report %p", cur, ok, r2)
	}
}

// TestSharedAnalysis_AReplicasOlderRunIsNotServed: a replica whose own pair holds an older run serves the
// deployment's latest one, not its own.
func TestSharedAnalysis_AReplicasOlderRunIsNotServed(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 2)
	a, b := replicaAnalysis(t, c.Replicas[0]), replicaAnalysis(t, c.Replicas[1])
	if _, err := b.AnalyzeFlow(ctx, storedDoc("v1")); err != nil {
		t.Fatal(err)
	}
	r2, err := a.AnalyzeFlow(ctx, storedDoc("v2"))
	if err != nil {
		t.Fatal(err)
	}
	if cur, ok := b.CurrentReport(storedDoc("v2")); !ok || cur.ContentHash != r2.ContentHash {
		t.Errorf("B serves its own older run as the current one: ok=%v, content %q, want %q", ok, contentOf(cur), r2.ContentHash)
	}
}

// TestSharedAnalysis_BackgroundRunsShareTheirIdentityNotTheirReport: a sweep's runs move the pair on (so the diff of
// the next interactive run is against the swept one, as on one replica) but store no report in Redis; a replica that
// does not hold the swept run answers the current run from its own pair, as before.
func TestSharedAnalysis_BackgroundRunsShareTheirIdentityNotTheirReport(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 2)
	a, b := replicaAnalysis(t, c.Replicas[0]), replicaAnalysis(t, c.Replicas[1])
	r1, err := a.AnalyzeFlow(ctx, storedDoc("v1"))
	if err != nil {
		t.Fatal(err)
	}
	swept, err := b.AnalyzeFlowBackground(ctx, storedDoc("v2"))
	if err != nil {
		t.Fatal(err)
	}
	key := analysisPairKey.Key("path", replicaFlow)
	if c.Redis.HGet(key, "cur_body") != "" {
		t.Error("a background run stored its report in Redis")
	}
	if c.Redis.HGet(key, "prev_body") == "" {
		t.Error("the interactive run's report did not move to prev")
	}
	if cur, ok := b.CurrentReport(storedDoc("v2")); !ok || cur != swept {
		t.Errorf("B's current run = (%p, %v), want its own swept report %p", cur, ok, swept)
	}
	if cur, ok := a.CurrentReport(storedDoc("v2")); !ok || cur != r1 {
		t.Errorf("A's current run = (%p, %v), want its own (as before: the swept report is not stored)", cur, ok)
	}
	if prev, has := a.PreviousReport(storedDoc("v2")); !has || prev != r1 {
		t.Errorf("A's previous run = (%p, %v), want its own run %p (the one before the sweep)", prev, has, r1)
	}
	// An interactive analysis of the swept content stores the report the sweep left out.
	if _, err := b.AnalyzeFlow(ctx, storedDoc("v2")); err != nil {
		t.Fatal(err)
	}
	if c.Redis.HGet(key, "cur_body") == "" {
		t.Error("an interactive analysis of the swept run did not store its report")
	}
	if cur, ok := a.CurrentReport(storedDoc("v2")); !ok || cur.ContentHash != swept.ContentHash {
		t.Errorf("A's current run after the interactive analysis on B = (%p, %v), want the v2 run", cur, ok)
	}
}

// TestSharedAnalysis_AReportOverTheCapStaysOnItsReplica: a report that does not compress under the cap is not
// stored; the replicas answer from their own pairs, as before.
func TestSharedAnalysis_AReportOverTheCapStaysOnItsReplica(t *testing.T) {
	ctx := context.Background()
	saved := sharedReportCap
	sharedReportCap = 16
	t.Cleanup(func() { sharedReportCap = saved })
	c := redisreplica.New(t, 2)
	a, b := replicaAnalysis(t, c.Replicas[0]), replicaAnalysis(t, c.Replicas[1])
	r1, err := a.AnalyzeFlow(ctx, storedDoc("v1"))
	if err != nil {
		t.Fatal(err)
	}
	key := analysisPairKey.Key("path", replicaFlow)
	if c.Redis.HGet(key, "cur") == "" || c.Redis.HGet(key, "cur_body") != "" {
		t.Fatalf("pair = %q / %d bytes of body, want the run without its report", c.Redis.HGet(key, "cur"), len(c.Redis.HGet(key, "cur_body")))
	}
	if cur, ok := b.CurrentReport(storedDoc("v1")); ok {
		t.Errorf("B served the run %q, a report it does not hold and Redis does not store", contentOf(cur))
	}
	if cur, ok := a.CurrentReport(storedDoc("v1")); !ok || cur != r1 {
		t.Errorf("A's current run = (%p, %v), want its own %p", cur, ok, r1)
	}
}

// TestSharedAnalysis_APairPastItsTTLKeepsTheReplicasOwnPrevious: the shared pair expires (24 h); the next analysis
// starts it again with the current run only. A replica whose own pair holds that run still diffs against its own
// previous run, as before; one that does not hold it has no previous run to offer.
func TestSharedAnalysis_APairPastItsTTLKeepsTheReplicasOwnPrevious(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 2)
	a, b := replicaAnalysis(t, c.Replicas[0]), replicaAnalysis(t, c.Replicas[1])
	r1, err := a.AnalyzeFlow(ctx, storedDoc("v1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AnalyzeFlow(ctx, storedDoc("v2")); err != nil {
		t.Fatal(err)
	}
	c.Redis.FastForward(analysisPairTTL + time.Second)
	if _, err := a.AnalyzeFlow(ctx, storedDoc("v2")); err != nil {
		t.Fatal(err)
	}
	if c.Redis.HGet(analysisPairKey.Key("path", replicaFlow), "prev") != "" {
		t.Fatal("premise: the restarted pair knows a previous run")
	}
	if prev, has := a.PreviousReport(storedDoc("v2")); !has || prev != r1 {
		t.Errorf("A's previous run after the pair expired = (%p, %v), want its own %p", prev, has, r1)
	}
	if prev, has := b.PreviousReport(storedDoc("v2")); has {
		t.Errorf("B offers the previous run %q it never held", contentOf(prev))
	}
}

// TestSharedAnalysis_RedisDownFallsBackToEachReplicasOwnState: an outage costs the sharing only; every call answers
// from the replica's own pair and claims, as on one replica.
func TestSharedAnalysis_RedisDownFallsBackToEachReplicasOwnState(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 2)
	a, b := replicaAnalysis(t, c.Replicas[0]), replicaAnalysis(t, c.Replicas[1])
	c.Kill()
	r1, fresh := analyzeFresh(t, a, storedDoc("v1"))
	if !fresh {
		t.Error("with Redis down a first run on A is not fresh")
	}
	if _, fresh := analyzeFresh(t, a, storedDoc("v1")); fresh {
		t.Error("with Redis down A's own claim no longer holds")
	}
	if _, fresh := analyzeFresh(t, b, storedDoc("v1")); !fresh {
		t.Error("with Redis down B's own claim answers: the run is fresh there (one record per replica, as before)")
	}
	if cur, ok := a.CurrentReport(storedDoc("v1")); !ok || cur != r1 {
		t.Errorf("with Redis down A's current run = (%p, %v), want its own %p", cur, ok, r1)
	}
	if _, err := a.AnalyzeFlow(ctx, storedDoc("v2")); err != nil {
		t.Fatal(err)
	}
	if prev, has := a.PreviousReport(storedDoc("v2")); !has || prev != r1 {
		t.Errorf("with Redis down A's previous run = (%p, %v), want its own %p", prev, has, r1)
	}
}

// contentOf is r's content hash for a failure message; "" for none.
func contentOf(r *models.AnalysisReport) string {
	if r == nil {
		return ""
	}
	return r.ContentHash
}

// TestSharedAnalysis_ANewerOwnRunBeatsAnOlderSharedOne: a Redis timeout pauses the sharing; the user re-analyzes on
// B meanwhile, so B's own pair moves on (current v2, previous v1) while Redis still says v1. After the pause B serves
// its own newer run, not the older shared one it happens to hold as its previous run.
func TestSharedAnalysis_ANewerOwnRunBeatsAnOlderSharedOne(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 2)
	a, b := replicaAnalysis(t, c.Replicas[0]), replicaAnalysis(t, c.Replicas[1])
	if _, err := a.AnalyzeFlow(ctx, storedDoc("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AnalyzeFlow(ctx, storedDoc("v1")); err != nil {
		t.Fatal(err)
	}
	b.shared.pausedUntil.Store(time.Now().Add(time.Hour).UnixNano()) // a failed call paused B's sharing
	r2, err := b.AnalyzeFlow(ctx, storedDoc("v2"))
	if err != nil {
		t.Fatal(err)
	}
	b.shared.pausedUntil.Store(0) // the pause is over; Redis still holds v1 as current
	if cur, ok := b.CurrentReport(storedDoc("v2")); !ok || cur != r2 {
		t.Errorf("B's current report after the pause = (content %q, %v), want its own newer run %q", contentOf(cur), ok, r2.ContentHash)
	}
}

// TestSharedAnalysis_ADecodedReportIsKeptAndDecodedOnce: a replica that serves another's run decodes it once and keeps
// it in its report cache (charged to its byte budget); concurrent readers share the one decode.
func TestSharedAnalysis_ADecodedReportIsKeptAndDecodedOnce(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 2)
	a, b := replicaAnalysis(t, c.Replicas[0]), replicaAnalysis(t, c.Replicas[1])
	if _, err := a.AnalyzeFlow(ctx, storedDoc("v1")); err != nil {
		t.Fatal(err)
	}
	const readers = 8
	got := make(chan *models.AnalysisReport, readers)
	for i := 0; i < readers; i++ {
		go func() {
			r, _ := b.CurrentReport(storedDoc("v1"))
			got <- r
		}()
	}
	first := <-got
	for i := 1; i < readers; i++ {
		if r := <-got; r != first || r == nil {
			t.Fatalf("concurrent readers got different decodes (%p, %p): each decoded the body itself", first, r)
		}
	}
	if again, _ := b.CurrentReport(storedDoc("v1")); again != first {
		t.Errorf("a later read decoded the body again (%p, want the kept %p)", again, first)
	}
	if b.shared.runs.Cost() == 0 {
		t.Error("the decoded report is not kept, charged by its bytes")
	}
}

// TestSharedAnalysis_FoldersOfTwoTenantsWithTheSameFileNamesStayApart: a stored folder flow re-parsed (a cloud fix, a
// snapshot restore) carries the parser's StableID, a hash of its file names, which another organisation's folder of
// the same file names shares. The pair, the claims and the report cache are keyed by the stored flow and its tenant,
// so neither replica serves one organisation's report for the other's flow.
func TestSharedAnalysis_FoldersOfTwoTenantsWithTheSameFileNamesStayApart(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 2)
	a, b := replicaAnalysis(t, c.Replicas[0]), replicaAnalysis(t, c.Replicas[1])
	folder := func(id, org string) *models.FlowDocument {
		d := storedDoc("v1")
		d.ID, d.StableID, d.OrganizationID, d.OwnerID, d.IsFolder = id, "files-0123456789abcdef", org, "owner-"+org, true
		return d
	}
	mine, theirs := folder("flow-org1", "org1"), folder("flow-org2", "org2")
	if _, fresh, err := a.AnalyzeFlowFresh(ctx, mine); err != nil || !fresh {
		t.Fatalf("org1's first run = fresh %v, %v", fresh, err)
	}
	if r, ok := a.CurrentReport(theirs); ok {
		t.Errorf("A serves org1's report (%q) for org2's flow", contentOf(r))
	}
	if r, ok := b.CurrentReport(theirs); ok {
		t.Errorf("B serves org1's shared report (%q) for org2's flow", contentOf(r))
	}
	if _, fresh, err := b.AnalyzeFlowFresh(ctx, theirs); err != nil || !fresh {
		t.Errorf("org2's first run = fresh %v, %v: org1's claim decided it", fresh, err)
	}
}

// TestSharedAnalysis_ThePairNeverMovesBackToAnOlderRecord: two replicas recording at once can land out of order; the
// record made earlier never replaces the later one, a repeat of the current run moves its time on, and a newer record
// of another run (content changed back) moves the pair as before.
func TestSharedAnalysis_ThePairNeverMovesBackToAnOlderRecord(t *testing.T) {
	ctx := context.Background()
	c := redisreplica.New(t, 1)
	a := replicaAnalysis(t, c.Replicas[0])
	r1, err := a.AnalyzeFlowReadOnly(ctx, storedDoc("v1"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.AnalyzeFlowReadOnly(ctx, storedDoc("v2"))
	if err != nil {
		t.Fatal(err)
	}
	k := analysisRecordKey(storedDoc("v1"))
	key := analysisPairKey.Key(k.tenant, k.flow)
	expect := func(step string, cur, prev *models.AnalysisReport, at string) {
		t.Helper()
		if got := c.Redis.HGet(key, "cur"); got != runIdentity(cur) {
			t.Errorf("%s: cur = %q, want %q", step, got, runIdentity(cur))
		}
		if prev != nil && c.Redis.HGet(key, "prev") != runIdentity(prev) {
			t.Errorf("%s: prev = %q, want %q", step, c.Redis.HGet(key, "prev"), runIdentity(prev))
		}
		if got := c.Redis.HGet(key, "cur_at"); got != at {
			t.Errorf("%s: cur_at = %q, want %q", step, got, at)
		}
	}
	a.shared.record(ctx, k, r2, 2000, true)
	a.shared.record(ctx, k, r1, 1000, true) // made before r2's, landing after it
	expect("an older record landing late", r2, nil, "2000")
	a.shared.record(ctx, k, r2, 3000, false)
	expect("a repeat of the current run", r2, nil, "3000")
	a.shared.record(ctx, k, r1, 2500, true)
	expect("a record older than the repeat", r2, nil, "3000")
	a.shared.record(ctx, k, r1, 4000, true)
	expect("a newer record of another run", r1, r2, "4000")
}
