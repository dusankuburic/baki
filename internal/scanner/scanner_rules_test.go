package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/notify"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Rule sets on baselines and snapshots, and the "new rules" notices (ADR 0016): findings of rules newer than a
// flow's records raise one notice per scope instead of drift and regression alerts, and preview rules raise nothing.

// ruleSetOf is the rule set of the named built-ins as the engine records it (each rule's revision and stage from
// the registry).
func ruleSetOf(t *testing.T, ids ...string) *models.RuleSet {
	t.Helper()
	byID := map[string]analyzer.Rule{}
	for _, r := range analyzer.AllRules() {
		byID[r.ID()] = r
	}
	refs := make([]models.RuleRef, 0, len(ids))
	for _, id := range ids {
		r, ok := byID[id]
		if !ok {
			t.Fatalf("no built-in rule %q", id)
		}
		refs = append(refs, analyzer.RuleRefOf(r))
	}
	return analyzer.NewRuleSet(refs)
}

func finding(rule, block string, sev models.Severity) models.Finding {
	return models.Finding{RuleID: rule, BlockID: block, Severity: sev}
}

// reportUnder is an analysis that ran current: its health is the engine's formula over its findings.
func reportUnder(flowID string, current *models.RuleSet, fs ...models.Finding) *models.AnalysisReport {
	return &models.AnalysisReport{FlowID: flowID, Findings: fs, RuleSet: current,
		Metrics: &models.FlowMetrics{HealthScore: analyzer.HealthOf(fs, nil)}}
}

// tenants seeds users and organisations (the notices' foreign keys).
func tenants(t *testing.T, b storageif.StorageBackend, users []string, orgs map[string][]string) {
	t.Helper()
	ctx := context.Background()
	for _, u := range users {
		if err := b.CreateUser(ctx, &storageif.User{ID: u, Email: u + "@example.com", Password: "hash"}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	for id, members := range orgs {
		org := &storageif.Organisation{ID: id, Name: id, OwnerID: members[0]}
		for _, m := range members {
			org.Members = append(org.Members, storageif.OrgMember{UserID: m, Role: auth.RoleMember})
		}
		if err := b.SaveOrg(ctx, org); err != nil {
			t.Fatalf("SaveOrg %s: %v", id, err)
		}
	}
}

func seedOwnedFlow(t *testing.T, b storageif.StorageBackend, id, owner, orgID string) {
	t.Helper()
	if err := b.SaveFlow(context.Background(), &storageif.FlowDocument{ID: id, Name: id, OwnerID: owner, OrganizationID: orgID,
		Content: []byte(`{"id":"` + id + `","subflows":[]}`)}); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
}

func setBaselineWith(t *testing.T, b storageif.StorageBackend, flowID string, rs *models.RuleSet, keys ...string) {
	t.Helper()
	if keys == nil {
		keys = []string{}
	}
	if err := b.SetFlowBaseline(context.Background(), &storageif.FlowBaseline{FlowID: flowID, Keys: keys, RuleSet: rs}); err != nil {
		t.Fatal(err)
	}
}

func setSnapshotWith(t *testing.T, b storageif.StorageBackend, flowID string, rs *models.RuleSet, fs ...models.Finding) {
	t.Helper()
	if err := b.SaveFlowAnalysis(context.Background(), &storageif.FlowAnalysis{FlowID: flowID,
		HealthScore: analyzer.HealthOf(fs, nil), RuleSet: rs}); err != nil {
		t.Fatal(err)
	}
}

// noticePushes returns the new_rules SSE pushes per user.
func noticePushes(sse *fakeSSE) map[string]int {
	out := map[string]int{}
	for _, c := range sse.calls() {
		if m, ok := c.data.(map[string]any); ok && m["type"] == string(notify.EventNewRules) {
			out[c.userID]++
		}
	}
	return out
}

func allNotices(t *testing.T, b *healthBackend) map[string]*storageif.RuleNotice {
	t.Helper()
	return b.RuleNoticesSnapshot()
}

// TestScanOnce_NewStableRuleRaisesOneNoticePerScope is the release scenario: a rule the flows' baselines and
// snapshots never saw reports errors in every flow of two organisations and one user's own flows. No drift, no
// regression, no channel delivery: three notices (one per scope), announced once to their audiences, and neither a
// second sweep nor a restart announces anything again.
func TestScanOnce_NewStableRuleRaisesOneNoticePerScope(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	tenants(t, b, []string{"owner-a", "member-a", "owner-b", "solo"},
		map[string][]string{"org-a": {"owner-a", "member-a"}, "org-b": {"owner-b"}})
	old := ruleSetOf(t, "unhandled-error", "wait-zero")
	current := ruleSetOf(t, "unhandled-error", "wait-zero", "hardcoded-credential")

	known := finding("unhandled-error", "b1", models.SeverityWarning)
	reports := map[string]*models.AnalysisReport{}
	for _, f := range []struct{ id, owner, org string }{
		{"a1", "owner-a", "org-a"}, {"a2", "owner-a", "org-a"}, {"b1", "owner-b", "org-b"}, {"p1", "solo", ""},
	} {
		seedOwnedFlow(t, b, f.id, f.owner, f.org)
		setBaselineWith(t, b, f.id, old, "unhandled-error:b1")
		setSnapshotWith(t, b, f.id, old, known)
		reports[f.id] = reportUnder(f.id, current, known, finding("hardcoded-credential", "b9", models.SeverityError))
	}
	// A flow with a snapshot and no baseline contributes too.
	seedOwnedFlow(t, b, "s1", "owner-a", "org-a")
	setSnapshotWith(t, b, "s1", old, known)
	reports["s1"] = reportUnder("s1", current, known, finding("hardcoded-credential", "b7", models.SeverityError))

	sweepOnce := func() (*capture, *fakeSSE) {
		cap := newCapture(t)
		sse := &fakeSSE{}
		s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
		s.SetEventNotifier(sse)
		s.ScanOnce(ctx)
		s.ScanOnce(ctx) // a second sweep of the same process
		s.Stop()
		return cap, sse
	}

	cap, sse := sweepOnce()
	if got := cap.all(); len(got) != 0 {
		t.Fatalf("channel deliveries = %+v, want none", got)
	}
	if alerts, _ := b.ListGovernanceAlerts(ctx, storageif.GovernanceAlertFilter{}); len(alerts) != 0 {
		t.Fatalf("inbox alerts = %+v, want none", alerts)
	}
	notices := allNotices(t, b)
	want := map[string]int{
		storageif.RuleNoticeID("org", "org-a", current.Digest): 3,
		storageif.RuleNoticeID("org", "org-b", current.Digest): 1,
		storageif.RuleNoticeID("user", "solo", current.Digest): 1,
	}
	if len(notices) != len(want) {
		t.Fatalf("notices = %v, want %d", notices, len(want))
	}
	for id, flows := range want {
		n := notices[id]
		if n == nil || n.FlowCount != flows || !slices.Equal(n.RuleIDs, []string{"hardcoded-credential"}) {
			t.Errorf("notice %s = %+v, want hardcoded-credential in %d flow(s)", id, n, flows)
		}
	}
	if got, want := noticePushes(sse), map[string]int{"owner-a": 1, "member-a": 1, "owner-b": 1, "solo": 1}; !mapsEqual(got, want) {
		t.Errorf("notice pushes = %v, want %v (one per new notice and member)", got, want)
	}

	// A restart: a new process re-scans every flow; the notices exist, so nothing is announced.
	cap, sse = sweepOnce()
	if got := cap.all(); len(got) != 0 {
		t.Errorf("restart: channel deliveries = %+v, want none", got)
	}
	if got := noticePushes(sse); len(got) != 0 {
		t.Errorf("restart: notice pushes = %v, want none", got)
	}
	if n := len(allNotices(t, b)); n != 3 {
		t.Errorf("restart: %d notices, want 3", n)
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestScanOnce_NewPreviewRuleRaisesNothing: the acceptance test of Phase 4: a preview rule new to every record
// reports errors, and raises no drift, no regression and no notice.
func TestScanOnce_NewPreviewRuleRaisesNothing(t *testing.T) {
	ctx := context.Background()
	m, ok := analyzer.LookupMeta("wait-zero")
	if !ok {
		t.Fatal("wait-zero is not registered")
	}
	m.Stage = analyzer.StagePreview
	t.Cleanup(analyzer.SetRuleMetaForTesting("wait-zero", m))

	b := newBackend()
	tenants(t, b, []string{"owner"}, map[string][]string{"org-1": {"owner"}})
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "wait-zero")
	if !slices.Contains(current.Rules, models.RuleRef{ID: "wait-zero", Stage: "preview"}) {
		t.Fatalf("precondition: wait-zero is not recorded as preview: %+v", current.Rules)
	}
	known := finding("unhandled-error", "b1", models.SeverityWarning)
	preview := finding("wait-zero", "b2", models.SeverityError)
	preview.Stage = "preview"
	seedOwnedFlow(t, b, "f1", "owner", "org-1")
	setBaselineWith(t, b, "f1", old, "unhandled-error:b1")
	setSnapshotWith(t, b, "f1", old, known)
	reports := map[string]*models.AnalysisReport{"f1": reportUnder("f1", current, known, preview)}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(ctx)
	if got := cap.all(); len(got) != 0 {
		t.Errorf("a preview rule raised %+v", got)
	}
	if n := allNotices(t, b); len(n) != 0 {
		t.Errorf("a preview rule raised notices %v", n)
	}
}

// TestScanOnce_DriftBesideANewRuleCountsTheKnownRulesOnly: genuine drift still alerts, its counts and signature
// without the new rule's findings, and the new rule still goes to the notice.
func TestScanOnce_DriftBesideANewRuleCountsTheKnownRulesOnly(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	tenants(t, b, []string{"owner"}, nil)
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-credential")
	seedOwnedFlow(t, b, "f1", "owner", "")
	setBaselineWith(t, b, "f1", old, "unhandled-error:b1")
	reports := map[string]*models.AnalysisReport{"f1": reportUnder("f1", current,
		finding("unhandled-error", "b1", models.SeverityWarning),
		finding("unhandled-error", "b2", models.SeverityError),
		finding("hardcoded-credential", "b9", models.SeverityError),
		finding("hardcoded-credential", "b8", models.SeverityWarning))}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(ctx)
	got := cap.all()
	if len(got) != 1 || got[0].Type != notify.EventDrift || got[0].NewErrors != 1 || got[0].NewWarnings != 0 ||
		!strings.HasPrefix(got[0].Message, "1 new finding(s)") {
		t.Fatalf("alerts = %+v, want one drift of 1 error", got)
	}
	if n := allNotices(t, b)[storageif.RuleNoticeID("user", "owner", current.Digest)]; n == nil || n.FlowCount != 1 {
		t.Fatalf("notice = %+v, want the new rule's", n)
	}
}

// TestScanOnce_LegacyRecords: a baseline and a snapshot written before rule sets were recorded know the built-ins
// of rule-set version 1 at their first revision. A revised rule (hardcoded-url, revision 2) is new to them: a notice
// and no drift (a revision may change its fingerprints). The snapshot's score counted its first revision, so its
// findings count in the current score too: a flow with a new finding of it regresses like any other.
func TestScanOnce_LegacyRecords(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	tenants(t, b, []string{"owner"}, nil)
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	known := finding("unhandled-error", "b1", models.SeverityWarning)
	revised := finding("hardcoded-url", "b3", models.SeverityWarning)

	// regressed: the known rule's new error is drift; revised: only the revised rule's finding is new, no drift.
	for _, id := range []string{"regressed", "revised"} {
		seedOwnedFlow(t, b, id, "owner", "")
		setBaselineWith(t, b, id, nil, "unhandled-error:b1")
		setSnapshotWith(t, b, id, nil, known)
	}
	reports := map[string]*models.AnalysisReport{
		"regressed": reportUnder("regressed", current, known, finding("unhandled-error", "b2", models.SeverityError), revised),
		"revised":   reportUnder("revised", current, known, revised),
	}
	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(ctx)

	byType := map[notify.EventType][]notify.Event{}
	for _, ev := range cap.all() {
		byType[ev.Type] = append(byType[ev.Type], ev)
	}
	drift, regr := byType[notify.EventDrift], byType[notify.EventHealthRegression]
	if len(drift) != 1 || drift[0].FlowID != "regressed" || drift[0].NewErrors != 1 || drift[0].NewWarnings != 0 {
		t.Errorf("drift alerts = %+v, want one for regressed with the known rule's error only", drift)
	}
	// A legacy snapshot was scored under the previous release's severities: it is not compared (modes-1, the
	// first sweep after the upgrade alerted on unchanged flows), though both scores dropped.
	for _, id := range []string{"regressed", "revised"} {
		if prior, to := analyzer.HealthOf([]models.Finding{known}, nil), reports[id].Metrics.HealthScore; to >= prior {
			t.Fatalf("precondition: %s's score %d does not drop below the snapshot's %d", id, to, prior)
		}
	}
	if len(regr) != 0 {
		t.Errorf("regression alerts = %+v, want none against a legacy snapshot", regr)
	}
	n := allNotices(t, b)[storageif.RuleNoticeID("user", "owner", current.Digest)]
	if n == nil || !slices.Equal(n.RuleIDs, []string{"hardcoded-url"}) || n.FlowCount != 2 {
		t.Fatalf("notice = %+v, want hardcoded-url in both flows", n)
	}
}

// TestScanOnce_RegressionLeavesOutOnlyTheRulesTheSnapshotDidNotScore: a snapshot scored with a known rule's warning
// and three warnings of hardcoded-url at its first revision (84); then the known rule reports a new error (72). The
// snapshot's score counted hardcoded-url, so the current score keeps it: leaving it out of the current score only
// (84 against 84) hid the regression. A rule the snapshot did not score (absent from its set, recorded as a preview
// rule, or introduced after a legacy record) is left out: its findings alone raise none, and beside a known rule's
// regression the message adds the full score. A legacy snapshot is not compared at all (another release's grading).
func TestScanOnce_RegressionLeavesOutOnlyTheRulesTheSnapshotDidNotScore(t *testing.T) {
	ctx := context.Background()
	known := finding("unhandled-error", "b1", models.SeverityWarning)
	before := []models.Finding{known, finding("hardcoded-url", "u1", models.SeverityWarning),
		finding("hardcoded-url", "u2", models.SeverityWarning), finding("hardcoded-url", "u3", models.SeverityWarning)}
	with := func(fs ...models.Finding) []models.Finding { return append(slices.Clone(before), fs...) }
	knownError := finding("unhandled-error", "b2", models.SeverityError)
	waitZero := finding("wait-zero", "w1", models.SeverityError)
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url", "wait-zero")
	if !slices.Contains(current.Rules, models.RuleRef{ID: "hardcoded-url", Rev: "r2"}) {
		t.Fatalf("precondition: hardcoded-url is not at revision 2: %+v", current.Rules)
	}
	firstRevision := analyzer.NewRuleSet([]models.RuleRef{{ID: "unhandled-error"}, {ID: "hardcoded-url"}, {ID: "wait-zero"}})
	withoutWaitZero := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	waitZeroPreview := analyzer.NewRuleSet([]models.RuleRef{{ID: "unhandled-error"}, {ID: "hardcoded-url", Rev: "r2"},
		{ID: "wait-zero", Stage: "preview"}})

	for _, tc := range []struct {
		name          string
		recorded      *models.RuleSet
		laterWaitZero bool // wait-zero introduced in rule-set version 2
		after         []models.Finding
		regressTo     int // 0: no alert
		full          int // the full score the message adds, 0: none
	}{
		{name: "legacy: not compared", after: with(knownError)},
		{name: "first revision recorded", recorded: firstRevision, after: with(knownError), regressTo: 72},
		{name: "rule absent from the set", recorded: withoutWaitZero, after: with(waitZero)},
		{name: "rule absent from the set, known rule regresses", recorded: withoutWaitZero, after: with(waitZero, knownError),
			regressTo: 72, full: 60},
		{name: "rule recorded as preview", recorded: waitZeroPreview, after: with(waitZero)},
		{name: "legacy, rule introduced later", laterWaitZero: true, after: with(waitZero)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.laterWaitZero {
				m, _ := analyzer.LookupMeta("wait-zero")
				m.IntroducedIn = 2
				t.Cleanup(analyzer.SetRuleMetaForTesting("wait-zero", m))
			}
			b := newBackend()
			tenants(t, b, []string{"owner"}, nil)
			seedOwnedFlow(t, b, "f1", "owner", "")
			setSnapshotWith(t, b, "f1", tc.recorded, before...)
			if h := b.health["f1"].HealthScore; h != 84 {
				t.Fatalf("precondition: snapshot health %d, want 84", h)
			}
			cap := newCapture(t)
			reports := map[string]*models.AnalysisReport{"f1": reportUnder("f1", current, tc.after...)}
			s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
			s.ScanOnce(ctx)

			got := cap.all()
			if tc.regressTo == 0 {
				if len(got) != 0 {
					t.Fatalf("alerts = %+v, want none: only a rule the snapshot did not score reports findings", got)
				}
				return
			}
			msg := fmt.Sprintf("Health score dropped from 84 to %d.", tc.regressTo)
			if tc.full != 0 {
				msg = fmt.Sprintf("Health score dropped from 84 to %d over the rules it was last recorded with (%d with the newer ones).", tc.regressTo, tc.full)
			}
			if len(got) != 1 || got[0].Type != notify.EventHealthRegression || got[0].PrevHealth != 84 ||
				got[0].HealthScore != tc.regressTo || got[0].Message != msg {
				t.Fatalf("alerts = %+v, want one regression 84 -> %d: %q", got, tc.regressTo, msg)
			}
		})
	}
}

// TestScanOnce_ACutSweepKeepsItsNoticeAndTheNextOneMergesIntoIt: a sweep whose context ends after the first flow
// still stores that flow's notice; the next sweep adds another flow's new rule to the same notice without
// announcing it again.
func TestScanOnce_ACutSweepKeepsItsNoticeAndTheNextOneMergesIntoIt(t *testing.T) {
	b := newBackend()
	tenants(t, b, []string{"owner"}, map[string][]string{"org-1": {"owner"}})
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-credential", "hardcoded-url")
	for _, id := range []string{"c1", "c2"} {
		seedOwnedFlow(t, b, id, "owner", "org-1")
		setBaselineWith(t, b, id, old)
	}
	reports := map[string]*models.AnalysisReport{
		"c1": reportUnder("c1", current, finding("hardcoded-credential", "b1", models.SeverityError)),
		"c2": reportUnder("c2", current, finding("hardcoded-url", "b1", models.SeverityWarning)),
	}
	sweepCtx, cut := context.WithCancel(context.Background())
	var once sync.Once
	analyze := func(_ context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
		once.Do(cut) // the tick ends right after the first flow
		return reports[doc.ID], nil
	}
	s := New(b, nil, analyze, nil, 0, loopbackChannels(b))
	sse := &fakeSSE{}
	s.SetEventNotifier(sse)
	s.ScanOnce(sweepCtx)

	id := storageif.RuleNoticeID("org", "org-1", current.Digest)
	if n := allNotices(t, b)[id]; n == nil || !slices.Equal(n.RuleIDs, []string{"hardcoded-credential"}) {
		t.Fatalf("after the cut sweep: notice %+v, want c1's rule stored", n)
	}
	if got := noticePushes(sse); got["owner"] != 1 {
		t.Fatalf("after the cut sweep: pushes %v, want one", got)
	}

	s.ScanOnce(context.Background())
	if n := allNotices(t, b)[id]; n == nil || !slices.Equal(n.RuleIDs, []string{"hardcoded-credential", "hardcoded-url"}) || n.FlowCount != 2 {
		t.Fatalf("after the next sweep: notice %+v, want both rules in 2 flows", n)
	}
	if got := noticePushes(sse); got["owner"] != 1 {
		t.Errorf("after the next sweep: pushes %v, want still one", got)
	}
}

// TestScanOnce_RecordedRuleSetsAreReadOncePerPage: the batch reads carry digests only; a sweep resolves a page's
// recorded rule sets in at most one read and reuses them for the rest of the sweep, never reads the current set,
// and an unchanged sweep reads none.
func TestScanOnce_RecordedRuleSetsAreReadOncePerPage(t *testing.T) {
	b := newBackend()
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	const flows = 150 // two pages
	h := newSkipHarness(t, b)
	for i := range flows {
		id := fmt.Sprintf("flow-%03d", i)
		seedFlow(t, b, id)
		recorded := old
		if i%2 == 1 {
			recorded = current // the current set needs no read
		}
		fs := []models.Finding{finding("unhandled-error", "b1", models.SeverityWarning), finding("unhandled-error", "b2", models.SeverityWarning)}
		setBaselineWith(t, b, id, recorded, "unhandled-error:b1")
		setSnapshotWith(t, b, id, recorded, fs...)
		h.reports[id] = reportUnder(id, current, fs...)
	}

	before := b.RuleSetsByDigestCalls.Load()
	sweep(h)
	if n := b.RuleSetsByDigestCalls.Load() - before; n != 1 {
		t.Fatalf("first sweep read rule sets %d times, want 1 (the old set, once, then cached)", n)
	}
	if got := len(h.cap.all()); got != flows {
		t.Fatalf("%d drift alerts, want %d", got, flows)
	}
	before = b.RuleSetsByDigestCalls.Load()
	sweep(h)
	if n := b.RuleSetsByDigestCalls.Load() - before; n != 0 {
		t.Fatalf("an unchanged sweep read rule sets %d times, want 0", n)
	}
}

// TestScanOnce_AnAlertAlreadyInTheInboxIsNotDeliveredAgain: after a restart (or on the replica that takes the
// sweep over), the in-memory dedup is empty; an alert already in the inbox was delivered when it was recorded, so it
// is not pushed or dispatched again.
func TestScanOnce_AnAlertAlreadyInTheInboxIsNotDeliveredAgain(t *testing.T) {
	ctx := context.Background()
	b, reports := alertingFlows(t, 3)
	first := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, first.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(ctx)
	if n := len(first.all()); n != 3 {
		t.Fatalf("first process delivered %d alerts, want 3", n)
	}

	restarted := newCapture(t)
	sse := &fakeSSE{}
	s2 := New(b, nil, analyzeReturning(reports), mustNotifier(t, restarted.srv.URL), 0, loopbackChannels(b))
	s2.SetEventNotifier(sse)
	s2.ScanOnce(ctx)
	if n := len(restarted.all()); n != 0 {
		t.Errorf("the restarted process delivered %d alerts again, want 0", n)
	}
	if n := len(sse.calls()); n != 0 {
		t.Errorf("the restarted process pushed %d alerts again, want 0", n)
	}
	if alerts, _ := b.ListGovernanceAlerts(ctx, storageif.GovernanceAlertFilter{}); len(alerts) != 3 {
		t.Errorf("%d inbox rows, want 3", len(alerts))
	}
}

// insertFailingBackend fails InsertGovernanceAlert while failInserts > 0.
type insertFailingBackend struct {
	*healthBackend
	mu          sync.Mutex
	failInserts int
}

func (b *insertFailingBackend) InsertGovernanceAlert(ctx context.Context, a *storageif.GovernanceAlert) (bool, error) {
	b.mu.Lock()
	fail := b.failInserts > 0
	if fail {
		b.failInserts--
	}
	b.mu.Unlock()
	if fail {
		return false, errors.New("inbox down")
	}
	return b.healthBackend.InsertGovernanceAlert(ctx, a)
}

// TestScanOnce_AnAlertTheInboxCouldNotRecordIsRetried: an alert is delivered only once it is in the inbox; one the
// inbox refused is not delivered, the flow's scan is not recorded, and the next sweep records and delivers it.
func TestScanOnce_AnAlertTheInboxCouldNotRecordIsRetried(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	seedFlow(t, b, "f1")
	setBaselineWith(t, b, "f1", nil, "r1:b1")
	fb := &insertFailingBackend{healthBackend: b, failInserts: 1}
	h := newSkipHarness(t, b)
	h.s.backend = fb
	h.reports["f1"] = &models.AnalysisReport{FlowID: "f1", Findings: []models.Finding{
		finding("r1", "b1", models.SeverityWarning), finding("r2", "b2", models.SeverityError)}}

	h.s.ScanOnce(ctx)
	if n := len(h.cap.all()); n != 0 {
		t.Fatalf("delivered %d alerts the inbox refused, want 0", n)
	}
	if k := h.s.lastScanKey("f1"); k != "" {
		t.Fatal("the scan whose alert was not recorded was recorded")
	}
	h.s.ScanOnce(ctx)
	if got := h.cap.all(); len(got) != 1 || got[0].NewErrors != 1 {
		t.Fatalf("after the retry: %+v, want the drift once", got)
	}
	h.s.ScanOnce(ctx)
	if n := len(h.cap.all()); n != 1 {
		t.Fatalf("delivered %d alerts after a third sweep, want still 1", n)
	}
}

// ruleSetFailingBackend fails RuleSetsByDigest and UpsertRuleNotices while asked to.
type ruleSetFailingBackend struct {
	*healthBackend
	mu                       sync.Mutex
	failReads, failUpserts   int
	upsertCalls, upsertFails int
}

func (b *ruleSetFailingBackend) RuleSetsByDigest(ctx context.Context, digests []string) (map[string]*models.RuleSet, error) {
	b.mu.Lock()
	fail := b.failReads > 0
	if fail {
		b.failReads--
	}
	b.mu.Unlock()
	if fail {
		b.RuleSetsByDigestCalls.Add(1)
		return nil, errors.New("rule sets unreadable")
	}
	return b.healthBackend.RuleSetsByDigest(ctx, digests)
}

func (b *ruleSetFailingBackend) UpsertRuleNotices(ctx context.Context, ns []*storageif.RuleNotice) ([]string, error) {
	b.mu.Lock()
	b.upsertCalls++
	fail := b.failUpserts > 0
	if fail {
		b.failUpserts--
		b.upsertFails++
	}
	b.mu.Unlock()
	if fail {
		return nil, errors.New("notices down")
	}
	return b.healthBackend.UpsertRuleNotices(ctx, ns)
}

// TestScanOnce_AnUnreadableRuleSetDefersTheComparison: when a recorded rule set cannot be read, the flow raises
// nothing (its findings cannot be told apart) and its scan is not recorded; the next sweep compares it.
func TestScanOnce_AnUnreadableRuleSetDefersTheComparison(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	tenants(t, b, []string{"owner"}, nil)
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	seedOwnedFlow(t, b, "f1", "owner", "")
	setBaselineWith(t, b, "f1", old, "unhandled-error:b1")
	fb := &ruleSetFailingBackend{healthBackend: b, failReads: 1}
	h := newSkipHarness(t, b)
	h.s.backend = fb
	h.reports["f1"] = reportUnder("f1", current, finding("unhandled-error", "b1", models.SeverityWarning),
		finding("hardcoded-url", "b2", models.SeverityWarning))

	h.s.ScanOnce(ctx)
	if n := len(h.cap.all()); n != 0 || len(allNotices(t, b)) != 0 {
		t.Fatalf("an unreadable rule set raised %d alerts and %d notices, want none", n, len(allNotices(t, b)))
	}
	if h.s.lastScanKey("f1") != "" {
		t.Fatal("the scan without a comparison was recorded")
	}
	h.s.ScanOnce(ctx)
	if n := len(h.cap.all()); n != 0 {
		t.Errorf("the revised rule raised %d drift alerts, want 0", n)
	}
	if n := allNotices(t, b)[storageif.RuleNoticeID("user", "owner", current.Digest)]; n == nil {
		t.Error("the next sweep raised no notice")
	}
}

// TestScanOnce_ANoticeTheStoreRefusedIsStoredLater: a failed page flush keeps the notice pending for the next flush
// and forgets its flows' scans, so a later sweep notes them again should the retry fail too.
func TestScanOnce_ANoticeTheStoreRefusedIsStoredLater(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	tenants(t, b, []string{"owner"}, nil)
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	seedOwnedFlow(t, b, "f1", "owner", "")
	setBaselineWith(t, b, "f1", old)
	fb := &ruleSetFailingBackend{healthBackend: b, failUpserts: 2} // the page's flush and the sweep's last one
	h := newSkipHarness(t, b)
	h.s.backend = fb
	h.reports["f1"] = reportUnder("f1", current, finding("hardcoded-url", "b2", models.SeverityWarning))

	h.s.ScanOnce(ctx)
	if fb.upsertFails != 2 || len(allNotices(t, b)) != 0 {
		t.Fatalf("after the refused flushes: %d failures, notices %v", fb.upsertFails, allNotices(t, b))
	}
	if h.s.lastScanKey("f1") != "" {
		t.Fatal("a flow whose notice was not stored keeps its scan")
	}
	h.s.ScanOnce(ctx)
	if n := allNotices(t, b)[storageif.RuleNoticeID("user", "owner", current.Digest)]; n == nil || n.FlowCount != 1 {
		t.Fatalf("the next sweep stored %+v, want the notice", n)
	}
	if h.s.lastScanKey("f1") == "" {
		t.Fatal("the stored scan is not recorded")
	}
}

// TestScanOnce_ANoticeStillRaisedOutlivesTheRetentionWindow: the retention sweep removes a notice no sweep touched
// within the window, and a sweep skips unchanged flows. A skipped flow notes its last scan's note again, and the
// sweep's last flush touches the notice once (not per page); before, a long-running process let a raised, dismissed
// notice be purged, and the next restart inserted and announced it again. A notice no flow raises any more goes.
func TestScanOnce_ANoticeStillRaisedOutlivesTheRetentionWindow(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	tenants(t, b, []string{"owner", "member"}, map[string][]string{"org-1": {"owner", "member"}})
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	const flows = 150 // two pages
	fb := &ruleSetFailingBackend{healthBackend: b}
	h := newSkipHarness(t, b)
	h.s.backend = fb
	h.s.SetEventNotifier(&fakeSSE{})
	ids := make([]string, flows)
	for i := range ids {
		ids[i] = fmt.Sprintf("flow-%03d", i)
		seedOwnedFlow(t, b, ids[i], "owner", "org-1")
		setBaselineWith(t, b, ids[i], old)
		h.reports[ids[i]] = reportUnder(ids[i], current, finding("hardcoded-url", "b1", models.SeverityWarning))
	}
	id := storageif.RuleNoticeID("org", "org-1", current.Digest)
	h.s.ScanOnce(ctx)
	if n := allNotices(t, b)[id]; n == nil || n.FlowCount != flows {
		t.Fatalf("notice = %+v, want one raised by %d flows", n, flows)
	}
	if err := b.DismissRuleNotice(ctx, "member", id); err != nil {
		t.Fatal(err)
	}
	age := func() { b.RuleNotices[id].UpdatedAt = time.Now().AddDate(0, 0, -40) }

	age()
	upserts := fb.upsertCalls
	h.s.ScanOnce(ctx)
	if n := h.runsOf(ids[0]); n != 1 {
		t.Fatalf("an unchanged flow was analyzed %d times, want 1", n)
	}
	if n := fb.upsertCalls - upserts; n != 1 {
		t.Errorf("a sweep of unchanged flows stored notices %d times, want once", n)
	}
	if res, err := b.PurgeExpiredData(ctx, 30); err != nil || res.RuleNotices != 0 {
		t.Fatalf("retention sweep = %+v, %v; want the raised notice kept", res, err)
	}

	// A restart re-scans every flow: the notice exists and stays dismissed, so nothing is announced.
	restarted := newSkipHarness(t, b)
	restarted.reports = h.reports
	sse := &fakeSSE{}
	restarted.s.SetEventNotifier(sse)
	restarted.s.ScanOnce(ctx)
	if got := noticePushes(sse); len(got) != 0 {
		t.Errorf("restart: notice pushes %v, want none", got)
	}
	if n := allNotices(t, b)[id]; n == nil || n.DismissedAt == nil {
		t.Fatalf("restart: notice %+v, want it dismissed", n)
	}

	// Re-baselined under the current rules, the flows raise the notice no more: it is not touched, and goes.
	for _, f := range ids {
		setBaselineWith(t, b, f, current, "hardcoded-url:b1")
	}
	h.s.ScanOnce(ctx)
	h.s.ScanOnce(ctx)
	age()
	if res, err := b.PurgeExpiredData(ctx, 30); err != nil || res.RuleNotices != 1 {
		t.Fatalf("retention sweep = %+v, %v; want the notice no flow raises purged", res, err)
	}
}

// TestScanOnce_NoticeScopes: without accounts every notice is the deployment's, whatever the flow's owner, and no
// one is pushed; with accounts a flow neither owned nor in an organisation has no audience and raises none.
func TestScanOnce_NoticeScopes(t *testing.T) {
	ctx := context.Background()
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	run := func(single bool, owner string) (*healthBackend, *fakeSSE) {
		b := newBackend()
		seedOwnedFlow(t, b, "f1", owner, "")
		setBaselineWith(t, b, "f1", old)
		reports := map[string]*models.AnalysisReport{"f1": reportUnder("f1", current, finding("hardcoded-url", "b1", models.SeverityWarning))}
		s := New(b, nil, analyzeReturning(reports), nil, 0, loopbackChannels(b))
		sse := &fakeSSE{}
		s.SetEventNotifier(sse)
		s.SetSingleTenant(single)
		s.ScanOnce(ctx)
		return b, sse
	}

	b, sse := run(true, "local")
	n := allNotices(t, b)[storageif.RuleNoticeID("app", "", current.Digest)]
	if n == nil {
		t.Fatalf("single tenant: notices %v, want the deployment's", allNotices(t, b))
	}
	if got, _ := b.ListRuleNotices(ctx, "local", 0); len(got) != 1 {
		t.Errorf("the local user lists %d notices, want 1", len(got))
	}
	if got := noticePushes(sse); len(got) != 0 {
		t.Errorf("single tenant: pushes %v, want none", got)
	}

	b, _ = run(false, "")
	if got := allNotices(t, b); len(got) != 0 {
		t.Errorf("an unowned flow raised notices %v", got)
	}
}

// TestScanOnce_RealAnalyses_ANewRuleIsANoticeAndAPreviewRuleIsSilent runs the engine: a flow baselined and scored
// under every rule but one, then swept with that rule too. Stable, the rule's findings become one notice and no
// alert; preview, they raise nothing at all (Phase 4's acceptance test).
func TestScanOnce_RealAnalyses_ANewRuleIsANoticeAndAPreviewRuleIsSilent(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "core", "analyzer", "testdata", "mixed_flow.txt"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.ParseText(string(src), "mixed_flow.txt", int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	all := analyzer.AllRules()
	full := analyzer.RunAnalysis(doc, all, nil, nil)
	// The new rule: one whose findings gate and are not the parse-error fallback's.
	newRule := ""
	for _, f := range full.Findings {
		if f.RuleID != "parse-error" && f.Severity != models.SeverityInfo {
			newRule = f.RuleID
			break
		}
	}
	if newRule == "" {
		t.Fatal("precondition: the corpus flow has no finding to play the new rule")
	}
	var without []analyzer.Rule
	for _, r := range all {
		if r.ID() != newRule {
			without = append(without, r)
		}
	}

	for _, stage := range []analyzer.Stage{analyzer.StageStable, analyzer.StagePreview} {
		t.Run(string(stage), func(t *testing.T) {
			if stage == analyzer.StagePreview {
				m, _ := analyzer.LookupMeta(newRule)
				m.Stage = analyzer.StagePreview
				t.Cleanup(analyzer.SetRuleMetaForTesting(newRule, m))
			}
			ctx := context.Background()
			b := newBackend()
			tenants(t, b, []string{"owner"}, nil)
			content, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.SaveFlow(ctx, &storageif.FlowDocument{ID: "f1", Name: "Mixed", OwnerID: "owner", Content: content}); err != nil {
				t.Fatal(err)
			}
			before := analyzer.RunAnalysis(doc, without, nil, nil)
			var keys []string
			for _, f := range before.Findings {
				keys = append(keys, f.Fingerprint)
			}
			setBaselineWith(t, b, "f1", before.RuleSet, keys...)
			if err := b.SaveFlowAnalysis(ctx, &storageif.FlowAnalysis{FlowID: "f1", HealthScore: before.Metrics.HealthScore,
				RuleSet: before.RuleSet}); err != nil {
				t.Fatal(err)
			}

			cap := newCapture(t)
			s := New(b, nil, func(ctx context.Context, d *models.FlowDocument) (*models.AnalysisReport, error) {
				return analyzer.RunAnalysisCtx(ctx, d, all, nil, nil), nil
			}, mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
			s.ScanOnce(ctx)

			if got := cap.all(); len(got) != 0 {
				t.Fatalf("%s rule %s raised alerts %+v", stage, newRule, got)
			}
			notices := allNotices(t, b)
			if stage == analyzer.StagePreview {
				if len(notices) != 0 {
					t.Fatalf("a preview rule raised notices %v", notices)
				}
				return
			}
			if len(notices) != 1 {
				t.Fatalf("notices = %v, want one", notices)
			}
			for _, n := range notices {
				if !slices.Equal(n.RuleIDs, []string{newRule}) || n.Scope != "user" || n.ScopeID != "owner" || n.FlowCount != 1 {
					t.Errorf("notice = %+v, want %s in owner's one flow", n, newRule)
				}
			}
		})
	}
}

// TestScanOnce_NoticesAreStoredPageByPage: a sweep stores its notes after every page, so a process that stops
// mid-sweep loses at most one page of them; the notice is inserted (and announced) by the first page and merged by
// the second.
func TestScanOnce_NoticesAreStoredPageByPage(t *testing.T) {
	b := newBackend()
	tenants(t, b, []string{"owner"}, map[string][]string{"org-1": {"owner"}})
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	reports := map[string]*models.AnalysisReport{}
	const flows = 150 // two pages
	for i := range flows {
		id := fmt.Sprintf("flow-%03d", i)
		seedOwnedFlow(t, b, id, "owner", "org-1")
		setBaselineWith(t, b, id, old)
		reports[id] = reportUnder(id, current, finding("hardcoded-url", "b1", models.SeverityWarning))
	}
	fb := &ruleSetFailingBackend{healthBackend: b}
	s := New(fb, nil, analyzeReturning(reports), nil, 0, loopbackChannels(fb))
	sse := &fakeSSE{}
	s.SetEventNotifier(sse)
	s.ScanOnce(context.Background())

	if fb.upsertCalls != 2 {
		t.Fatalf("notices stored %d times, want 2 (once per page)", fb.upsertCalls)
	}
	n := allNotices(t, b)[storageif.RuleNoticeID("org", "org-1", current.Digest)]
	if n == nil || n.FlowCount != flows {
		t.Fatalf("notice = %+v, want all %d flows", n, flows)
	}
	if got := noticePushes(sse); got["owner"] != 1 {
		t.Errorf("pushes = %v, want one", got)
	}
}

// s2-2: the sweep that skips a legacy snapshot records its own analysis as the flow's snapshot, so the regression
// check resumes for flows nobody analyzes interactively: the next drop is compared like for like.
func TestScanOnce_LegacySnapshotIsReRecordedThenCompared(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	tenants(t, b, []string{"owner"}, nil)
	seedOwnedFlow(t, b, "f1", "owner", "")
	known := finding("unhandled-error", "b1", models.SeverityWarning)
	setSnapshotWith(t, b, "f1", nil, known)
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	first := reportUnder("f1", current, known, finding("unhandled-error", "b2", models.SeverityError))
	reports := map[string]*models.AnalysisReport{"f1": first}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
	recorded := 0
	s.SetHealthRecorder(func(ctx context.Context, doc *models.FlowDocument, r *models.AnalysisReport) {
		recorded++
		if err := b.SaveFlowAnalysis(ctx, &storageif.FlowAnalysis{FlowID: doc.ID, HealthScore: r.Metrics.HealthScore, RuleSet: r.RuleSet}); err != nil {
			t.Error(err)
		}
	})
	s.ScanOnce(ctx)
	if got := cap.all(); len(got) != 0 {
		t.Fatalf("alerts = %+v, want none against the legacy snapshot", got)
	}
	if h := b.health["f1"]; recorded != 1 || h == nil || h.RuleSetDigest != current.Digest || h.HealthScore != first.Metrics.HealthScore {
		t.Fatalf("recorded %d times, snapshot %+v; want the sweep's analysis under the current rule set", recorded, h)
	}

	second := reportUnder("f1", current, known, finding("unhandled-error", "b2", models.SeverityError),
		finding("unhandled-error", "b3", models.SeverityError))
	reports["f1"] = second
	s.ScanOnce(ctx)
	got := cap.all()
	if len(got) != 1 || got[0].Type != notify.EventHealthRegression || got[0].PrevHealth != first.Metrics.HealthScore ||
		got[0].HealthScore != second.Metrics.HealthScore {
		t.Fatalf("alerts = %+v, want one regression %d -> %d", got, first.Metrics.HealthScore, second.Metrics.HealthScore)
	}
	if recorded != 1 {
		t.Errorf("a recorded snapshot was re-recorded (%d calls): only a legacy one is", recorded)
	}
}
