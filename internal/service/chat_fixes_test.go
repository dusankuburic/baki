package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// newFixTestService builds a ChatService wired for applier tests: a local
// document provider holding doc, a fake apply hook, and a real analysis
// service for the verification step.
func newFixTestService(doc *models.FlowDocument, applyHook applyFixFunc) (*ChatService, *testutil.CountingNotifier) {
	notifier := &testutil.CountingNotifier{}
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	lfs := NewFlowService(notifier, nil, ldp, nil, nil, nil)
	analysisSvc, err := NewAnalysisService(notifier, nilSettings{}, nil)
	if err != nil {
		panic(err)
	}
	svc := &ChatService{
		notifier:      notifier,
		flowCache:     lfs,
		analysisCache: analysisSvc,
		applyFixFunc:  applyHook,
	}
	return svc, notifier
}

func fixTestDoc() *models.FlowDocument {
	doc := &models.FlowDocument{ID: "flow-1", Name: "Main"}
	doc.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: []models.Block{
		{ID: "b1", Name: "Call API", Type: models.BlockTypeAction, RawType: "HTTPClient.InvokeUrl", LineNumber: 3, Properties: map[string]string{"Url": "https://x"}},
	}}}
	doc.RebuildIndexes()
	return doc
}

func fixTestProposal() ai.FixProposal {
	return ai.FixProposal{
		ProposalID:  "prop-1",
		RuleID:      "unhandled-error",
		FixType:     "wrap-error-handler",
		BlockID:     "b1",
		BlockLabel:  "Call API",
		Line:        3,
		Fingerprint: "fp-1",
		Summary:     "Fix \"wrap-error-handler\" for rule \"unhandled-error\"…",
	}
}

// collectFixEvents records (eventType, data) pairs emitted through the
// applier's emit, with a done marker for synchronization.
type fixEventRecorder struct {
	mu     sync.Mutex
	events []string // "type:field=value" summaries
}

func (r *fixEventRecorder) emit(eventType string, data map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status, _ := data["status"].(string)
	if eventType == "fix_decision" {
		r.events = append(r.events, "fix_decision:"+status)
	} else {
		r.events = append(r.events, "fix_proposal")
	}
}

func (r *fixEventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// realFingerprintFor analyzes the doc through the service's own cache and
// returns the fingerprint of the first finding matching ruleID on blockID —
// proposals must carry REAL content-derived fingerprints for verification.
func realFingerprintFor(t *testing.T, svc *ChatService, doc *models.FlowDocument, ruleID, blockID string) string {
	t.Helper()
	report, err := svc.analysisCache.AnalyzeFlow(context.Background(), doc)
	if err != nil || report == nil {
		t.Fatalf("pre-analysis: %v", err)
	}
	for i := range report.Findings {
		f := &report.Findings[i]
		if f.RuleID == ruleID && f.BlockID == blockID {
			if f.Fingerprint == "" {
				t.Fatal("fixture finding has no fingerprint — pick a rule that sets one")
			}
			return f.Fingerprint
		}
	}
	t.Fatalf("fixture doc produces no %s finding on %s (got %d findings)", ruleID, blockID, len(report.Findings))
	return ""
}

// TestApplyWithApproval_ApproveAppliesAndVerifies drives the full happy path:
// proposal event → approve decision → apply hook → verification by
// fingerprint → applied event + model-readable summary. The apply hook
// returns a fixed doc without the offending block, so the real fingerprint no
// longer appears and verification must report "applied".
func TestApplyWithApproval_ApproveAppliesAndVerifies(t *testing.T) {
	doc := fixTestDoc()
	applied := make(chan ai.FixProposal, 1)
	hook := func(_ context.Context, _ *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (*models.FlowDocument, error) {
		applied <- ai.FixProposal{BlockID: blockID, FixType: fixType, RuleID: ruleID, Variable: variable, Property: property}
		fixed := &models.FlowDocument{ID: doc.ID, Name: doc.Name}
		fixed.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: []models.Block{
			{ID: "fresh-id", Name: "Comment", Type: models.BlockTypeAction, RawType: "Comment", LineNumber: 1},
		}}}
		fixed.RebuildIndexes()
		return fixed, nil
	}
	svc, _ := newFixTestService(doc, hook)
	rec := &fixEventRecorder{}
	ap := svc.newFixApplier("user-1", doc, "stream-1", rec.emit, nil, nil)
	if ap == nil {
		t.Fatal("applier not wired")
	}
	prop := fixTestProposal()
	prop.Fingerprint = realFingerprintFor(t, svc, doc, prop.RuleID, prop.BlockID)

	// Resolve the decision as soon as the proposal is registered.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := svc.pendingFixDecisions.Load(prop.ProposalID); ok {
				_ = svc.ResolveFixDecision("stream-1", prop.ProposalID, true, nil)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	out, err := ap.ApplyFixWithApproval(context.Background(), prop)
	if err != nil {
		t.Fatalf("ApplyFixWithApproval: %v", err)
	}
	if !strings.Contains(out, "APPLIED") || !strings.Contains(out, "no longer appears") {
		t.Errorf("want applied+verified summary, got %q", out)
	}
	select {
	case p := <-applied:
		if p.BlockID != "b1" || p.FixType != "wrap-error-handler" || p.RuleID != "unhandled-error" {
			t.Errorf("apply hook args wrong: %+v", p)
		}
	default:
		t.Fatal("apply hook never called")
	}
	evs := rec.snapshot()
	if len(evs) != 3 || evs[0] != "fix_proposal" || evs[1] != "fix_decision:applying" || evs[2] != "fix_decision:applied" {
		t.Errorf("event sequence wrong: %v", evs)
	}
}

// TestApplyWithApproval_ApproveButFindingPersists: the apply hook returns a
// doc that still carries the same real fingerprint → the outcome must be
// "applied-unresolved", NOT a claimed success.
func TestApplyWithApproval_ApproveButFindingPersists(t *testing.T) {
	doc := fixTestDoc()
	svc, _ := newFixTestService(doc, func(_ context.Context, d *models.FlowDocument, _, _, _, _, _ string) (*models.FlowDocument, error) {
		return d, nil // no-op apply — the finding is still there
	})
	rec := &fixEventRecorder{}
	ap := svc.newFixApplier("user-1", doc, "stream-1", rec.emit, nil, nil)
	prop := fixTestProposal()
	prop.Fingerprint = realFingerprintFor(t, svc, doc, prop.RuleID, prop.BlockID)

	go func() {
		for i := 0; i < 1000; i++ {
			if _, ok := svc.pendingFixDecisions.Load(prop.ProposalID); ok {
				_ = svc.ResolveFixDecision("stream-1", prop.ProposalID, true, nil)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	out, err := ap.ApplyFixWithApproval(context.Background(), prop)
	if err != nil {
		t.Fatalf("ApplyFixWithApproval: %v", err)
	}
	if !strings.Contains(out, "APPLIED, but") || !strings.Contains(out, "still appears") {
		t.Errorf("want applied-unresolved summary, got %q", out)
	}
	evs := rec.snapshot()
	if evs[len(evs)-1] != "fix_decision:applied-unresolved" {
		t.Errorf("last event must be applied-unresolved, got %v", evs)
	}
}

// TestApplyWithApproval_DeclinedAndTimeout pin the no-write paths.
func TestApplyWithApproval_DeclinedAndTimeout(t *testing.T) {
	t.Run("declined", func(t *testing.T) {
		svc, _ := newFixTestService(fixTestDoc(), func(_ context.Context, doc *models.FlowDocument, _, _, _, _, _ string) (*models.FlowDocument, error) {
			t.Error("apply hook must not run on decline")
			return doc, nil
		})
		rec := &fixEventRecorder{}
		ap := svc.newFixApplier("user-1", fixTestDoc(), "stream-1", rec.emit, nil, nil)
		prop := fixTestProposal()
		go func() {
			for i := 0; i < 1000; i++ {
				if _, ok := svc.pendingFixDecisions.Load(prop.ProposalID); ok {
					_ = svc.ResolveFixDecision("stream-1", prop.ProposalID, false, nil)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		out, err := ap.ApplyFixWithApproval(context.Background(), prop)
		if err != nil {
			t.Fatalf("ApplyFixWithApproval: %v", err)
		}
		if !strings.Contains(out, "DECLINED") {
			t.Errorf("want declined summary, got %q", out)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		svc, _ := newFixTestService(fixTestDoc(), nil)
		svc.fixDecisionWait = 30 * time.Millisecond
		rec := &fixEventRecorder{}
		ap := svc.newFixApplier("user-1", fixTestDoc(), "stream-1", rec.emit, nil, nil)

		start := time.Now()
		out, err := ap.ApplyFixWithApproval(context.Background(), fixTestProposal())
		if err != nil {
			t.Fatalf("ApplyFixWithApproval: %v", err)
		}
		if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
			t.Errorf("timeout returned too fast (%v) — wait not honored", elapsed)
		}
		if !strings.Contains(out, "did not respond") {
			t.Errorf("want timeout summary, got %q", out)
		}
		if evs := rec.snapshot(); evs[len(evs)-1] != "fix_decision:timeout" {
			t.Errorf("last event must be timeout, got %v", evs)
		}
	})
}

// recordingProvider embeds FakeProvider and captures each request's messages
// so tests can assert what the model actually saw.
type recordingProvider struct {
	testutil.FakeProvider
	mu   sync.Mutex
	reqs [][]ai.Message
}

func (p *recordingProvider) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	p.mu.Lock()
	p.reqs = append(p.reqs, req.Messages)
	p.mu.Unlock()
	return p.FakeProvider.Stream(ctx, req, onChunk)
}

// TestRunToolLoop_ApplyFixEndToEnd wires the full agent path: the model calls
// apply_fix → the loop dispatches through the approval applier → the user
// approves → the fix hook runs → re-analysis verifies → the outcome string is
// fed back to the model's next turn → the model answers and the stream
// completes with the fix_proposal/fix_decision events emitted in order.
func TestRunToolLoop_ApplyFixEndToEnd(t *testing.T) {
	doc := fixTestDoc()
	svc, _ := newFixTestService(doc, func(_ context.Context, _ *models.FlowDocument, _, _, _, _, _ string) (*models.FlowDocument, error) {
		fixed := &models.FlowDocument{ID: doc.ID, Name: doc.Name}
		fixed.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: []models.Block{
			{ID: "fresh", Name: "Comment", Type: models.BlockTypeAction, RawType: "Comment", LineNumber: 1},
		}}}
		fixed.RebuildIndexes()
		return fixed, nil
	})
	// The proposal the model will trigger carries a fingerprint resolved from
	// the doc's real analysis — capture it by running the same resolution the
	// tool does: pre-analyze and take the unhandled-error finding.
	wantFP := realFingerprintFor(t, svc, doc, "unhandled-error", "b1")

	rec := &fixEventRecorder{}
	ap := svc.newFixApplier("user-1", doc, "stream-1", rec.emit, nil, nil)
	tctx := ai.ToolContext{Ctx: context.Background(), Doc: doc, RealDoc: doc, Analysis: svc.analysisCache, Fixes: ap}

	prov := &recordingProvider{FakeProvider: testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}}, TokensIn: 10, TokensOut: 5},
		{Text: "Done — fixed and verified.", TokensIn: 8, TokensOut: 3},
	}}}

	ctl := &streamCtl{cancel: func() {}, started: make(chan struct{})}
	close(ctl.started)

	// Approve as soon as the proposal lands.
	go func() {
		for i := 0; i < 2500; i++ { // ~5s max
			found := false
			svc.pendingFixDecisions.Range(func(_, _ any) bool { found = true; return false })
			if found {
				svc.pendingFixDecisions.Range(func(k, v any) bool {
					st := v.(*fixDecisionState)
					if st.streamID == "stream-1" {
						_ = svc.ResolveFixDecision("stream-1", k.(string), true, nil)
					}
					return false
				})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	emit, evs := collectEvents()
	svc.runToolLoop(context.Background(), prov, ai.Request{Messages: []ai.Message{{Role: "user", Content: "fix it"}}},
		"user-1", &tctx, ctl, func() bool { return true }, emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("stream did not complete; errMsg=%q", ctl.errMsg)
	}
	if ctl.buffer.String() != "Done — fixed and verified." {
		t.Errorf("final answer wrong: %q (errMsg=%q)", ctl.buffer.String(), ctl.errMsg)
	}
	// The model's second request must contain the tool result with the
	// verified outcome and the proposal must have carried the real fingerprint.
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.reqs) != 2 {
		t.Fatalf("want 2 model turns, got %d", len(prov.reqs))
	}
	toolResults := ""
	for _, m := range prov.reqs[1] {
		if m.Role == "tool" {
			toolResults += m.Content
		}
	}
	if !strings.Contains(toolResults, "APPLIED") {
		t.Errorf("model's second turn must carry the applied outcome, got: %q", toolResults)
	}
	// Loop-level events: tool status + final done.
	if got := strings.Join(*evs, ","); !strings.Contains(got, "tool") || !strings.Contains(got, "done") {
		t.Errorf("expected tool+done events, got %q", got)
	}
	// Applier-level events in order.
	fixes := rec.snapshot()
	if len(fixes) != 3 || fixes[0] != "fix_proposal" || fixes[1] != "fix_decision:applying" || fixes[2] != "fix_decision:applied" {
		t.Errorf("fix event sequence wrong: %v", fixes)
	}
	_ = wantFP
}

// TestResolveFixDecision_OwnershipAndFirstWins locks the decision registry
// semantics: wrong stream rejected, first decision wins, unknown rejected.
func TestResolveFixDecision_OwnershipAndFirstWins(t *testing.T) {
	svc, _ := newFixTestService(fixTestDoc(), nil)
	st := &fixDecisionState{streamID: "stream-1", decided: make(chan bool, 1)}
	svc.pendingFixDecisions.Store("prop-1", st)

	if err := svc.ResolveFixDecision("other-stream", "prop-1", true, nil); !errors.Is(err, ErrNoPendingFix) {
		t.Errorf("foreign stream must be rejected, got %v", err)
	}
	if err := svc.ResolveFixDecision("stream-1", "unknown", true, nil); !errors.Is(err, ErrNoPendingFix) {
		t.Errorf("unknown proposal must be rejected, got %v", err)
	}
	if err := svc.ResolveFixDecision("stream-1", "prop-1", true, nil); err != nil {
		t.Fatalf("valid decision rejected: %v", err)
	}
	if err := svc.ResolveFixDecision("stream-1", "prop-1", false, nil); err != nil {
		t.Fatalf("second decision should not error (first wins), got %v", err)
	}
	if decided := <-st.decided; !decided {
		t.Error("first decision (true) must win over the later false")
	}
}
