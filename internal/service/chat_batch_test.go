package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// multiFixDoc carries TWO fixable HTTP blocks (two unhandled-error findings)
// so a batch has distinct targets.
func multiFixDoc() *models.FlowDocument {
	doc := &models.FlowDocument{ID: "flow-1", Name: "Main"}
	doc.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: []models.Block{
		{ID: "b1", Name: "Call API", Type: models.BlockTypeAction, RawType: "HTTPClient.InvokeUrl", LineNumber: 3, Properties: map[string]string{"Url": "https://x"}},
		{ID: "b2", Name: "Sync API", Type: models.BlockTypeAction, RawType: "HTTPClient.InvokeUrl", LineNumber: 8, Properties: map[string]string{"Url": "https://y"}},
	}}}
	doc.RebuildIndexes()
	return doc
}

// afterFix1Doc models the re-parse after fixing b1: fresh block IDs, b2's
// finding still present (on a new ID).
func afterFix1Doc() *models.FlowDocument {
	doc := &models.FlowDocument{ID: "flow-1", Name: "Main"}
	doc.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: []models.Block{
		{ID: "new-comment", Name: "Handled", Type: models.BlockTypeAction, RawType: "Comment", LineNumber: 3},
		{ID: "fresh-2", Name: "Sync API", Type: models.BlockTypeAction, RawType: "HTTPClient.InvokeUrl", LineNumber: 12, Properties: map[string]string{"Url": "https://y"}},
	}}}
	doc.RebuildIndexes()
	return doc
}

func afterFix2Doc() *models.FlowDocument {
	doc := &models.FlowDocument{ID: "flow-1", Name: "Main"}
	doc.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: []models.Block{
		{ID: "new-comment", Name: "Handled", Type: models.BlockTypeAction, RawType: "Comment", LineNumber: 3},
		{ID: "new-comment-2", Name: "Handled", Type: models.BlockTypeAction, RawType: "Comment", LineNumber: 12},
	}}}
	doc.RebuildIndexes()
	return doc
}

// TestApplyFixes_BatchEndToEnd: two targets, one approval, both applied and
// verified — the second target must be re-associated on the re-parsed flow
// (fresh block IDs, shifted lines) before its patch runs.
func TestApplyFixes_BatchEndToEnd(t *testing.T) {
	doc := multiFixDoc()
	var hookCalls []string
	hook := func(_ context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		hookCalls = append(hookCalls, blockID)
		switch blockID {
		case "b1":
			return afterFix1Doc(), nil
		default:
			return afterFix2Doc(), nil
		}
	}
	svc, _ := newFixTestService(doc, hook)

	props := []ai.FixProposal{
		{ProposalID: "p1", RuleID: "unhandled-error", FixType: "wrap-error-handler", BlockID: "b1", BlockLabel: "Call API", Fingerprint: realFingerprintFor(t, svc, doc, "unhandled-error", "b1")},
		{ProposalID: "p2", RuleID: "unhandled-error", FixType: "wrap-error-handler", BlockID: "b2", BlockLabel: "Sync API", Fingerprint: realFingerprintFor(t, svc, doc, "unhandled-error", "b2")},
	}

	rec := &batchEventRecorder{}
	ap := svc.newFixApplier("user-1", doc, "stream-1", rec.emit, nil, nil)

	// Approve the batch as soon as its decision key registers.
	go func() {
		for i := 0; i < 2500; i++ {
			approved := false
			svc.pendingFixDecisions.Range(func(k, v any) bool {
				st := v.(*fixDecisionState)
				if st.streamID == "stream-1" {
					_ = svc.ResolveFixDecision("stream-1", k.(string), true, nil)
					approved = true
					return false
				}
				return true
			})
			if approved {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	out, err := ap.ApplyFixesWithApproval(context.Background(), props)
	if err != nil {
		t.Fatalf("ApplyFixesWithApproval: %v", err)
	}
	if !strings.Contains(out, "Batch result: applied") {
		t.Errorf("want batch applied summary, got %q", out)
	}
	if !strings.Contains(out, "list_findings again") {
		t.Errorf("summary must tell the model block IDs changed: %q", out)
	}
	// Both fixes reached the hook: the first with the original block ID, the
	// second with the RE-PARSED block ID (re-association), not the stale "b2".
	if len(hookCalls) != 2 || hookCalls[0] != "b1" || hookCalls[1] != "fresh-2" {
		t.Errorf("hook calls = %v, want [b1 fresh-2] (second re-associated on the re-parse)", hookCalls)
	}

	evs := rec.snapshot()
	if len(evs) != 3 || evs[0].typ != "fix_proposal" || evs[1].typ != "fix_decision" || evs[1].status != "applying" || evs[2].typ != "fix_decision" || evs[2].status != "applied" {
		t.Fatalf("batch event sequence wrong: %+v", evs)
	}
	// The decision event carries per-item statuses.
	if len(evs[2].items) != 2 {
		t.Errorf("decision items missing: %+v", evs[2].items)
	}
	for _, it := range evs[2].items {
		if it["status"] != "applied" {
			t.Errorf("item %v not applied", it)
		}
	}
	// The proposal event is batch-shaped.
	if evs[0].batch != true || evs[0].count != 2 {
		t.Errorf("proposal event not batch-shaped: batch=%v count=%v", evs[0].batch, evs[0].count)
	}
}

// batchEventRecorder captures batch-shaped fix events.
type batchEventRecorder struct {
	events []batchEvent
}
type batchEvent struct {
	typ    string
	status string
	batch  bool
	count  int
	items  []map[string]interface{}
}

func (r *batchEventRecorder) emit(eventType string, data map[string]interface{}) {
	ev := batchEvent{typ: eventType}
	if eventType == "fix_proposal" {
		ev.batch, _ = data["batch"].(bool)
		if c, ok := data["count"].(int); ok {
			ev.count = c
		}
		if items, ok := data["items"].([]map[string]interface{}); ok {
			ev.items = items
		}
	} else {
		ev.status, _ = data["status"].(string)
		if items, ok := data["items"].([]map[string]interface{}); ok {
			ev.items = items
		}
	}
	r.events = append(r.events, ev)
}

func (r *batchEventRecorder) snapshot() []batchEvent {
	out := make([]batchEvent, len(r.events))
	copy(out, r.events)
	return out
}

// TestApplyFixes_DeclinedAndTimeout: no hook call, no batch event items.
func TestApplyFixes_DeclinedAndTimeout(t *testing.T) {
	t.Run("declined", func(t *testing.T) {
		doc := multiFixDoc()
		svc, _ := newFixTestService(doc, func(_ context.Context, d *models.FlowDocument, _, _, _, _, _ string) (*models.FlowDocument, error) {
			t.Error("hook must not run on decline")
			return d, nil
		})
		rec := &batchEventRecorder{}
		ap := svc.newFixApplier("user-1", doc, "stream-1", rec.emit, nil, nil)
		go func() {
			for i := 0; i < 1000; i++ {
				approved := false
				svc.pendingFixDecisions.Range(func(k, v any) bool {
					st := v.(*fixDecisionState)
					if st.streamID == "stream-1" {
						_ = svc.ResolveFixDecision("stream-1", k.(string), false, nil)
						approved = true
						return false
					}
					return true
				})
				if approved {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		out, err := ap.ApplyFixesWithApproval(context.Background(), []ai.FixProposal{fixTestProposal()})
		if err != nil || !strings.Contains(out, "DECLINED") {
			t.Fatalf("declined: out=%q err=%v", out, err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		svc, _ := newFixTestService(multiFixDoc(), nil)
		svc.fixDecisionWait = 30 * time.Millisecond
		rec := &batchEventRecorder{}
		ap := svc.newFixApplier("user-1", multiFixDoc(), "stream-1", rec.emit, nil, nil)
		out, err := ap.ApplyFixesWithApproval(context.Background(), []ai.FixProposal{fixTestProposal()})
		if err != nil || !strings.Contains(out, "did not respond") {
			t.Fatalf("timeout: out=%q err=%v", out, err)
		}
		evs := rec.snapshot()
		if evs[len(evs)-1].status != "timeout" {
			t.Errorf("last event must be timeout, got %+v", evs[len(evs)-1])
		}
	})
}

// TestApplyFixes_PartialFailure: item 1 applies, item 2's hook errors — the
// batch continues to verification and reports per-item outcomes; overall
// status is applied-unresolved (partial).
func TestApplyFixes_PartialFailure(t *testing.T) {
	doc := multiFixDoc()
	hook := func(_ context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		if blockID == "b1" {
			return afterFix1Doc(), nil
		}
		return nil, context.DeadlineExceeded
	}
	svc, _ := newFixTestService(doc, hook)
	rec := &batchEventRecorder{}
	ap := svc.newFixApplier("user-1", doc, "stream-1", rec.emit, nil, nil)
	props := []ai.FixProposal{
		{ProposalID: "p1", RuleID: "unhandled-error", FixType: "wrap-error-handler", BlockID: "b1", BlockLabel: "Call API", Fingerprint: realFingerprintFor(t, svc, doc, "unhandled-error", "b1")},
		{ProposalID: "p2", RuleID: "unhandled-error", FixType: "wrap-error-handler", BlockID: "b2", BlockLabel: "Sync API", Fingerprint: realFingerprintFor(t, svc, doc, "unhandled-error", "b2")},
	}
	go func() {
		for i := 0; i < 2500; i++ {
			approved := false
			svc.pendingFixDecisions.Range(func(k, v any) bool {
				st := v.(*fixDecisionState)
				if st.streamID == "stream-1" {
					_ = svc.ResolveFixDecision("stream-1", k.(string), true, nil)
					approved = true
					return false
				}
				return true
			})
			if approved {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	out, err := ap.ApplyFixesWithApproval(context.Background(), props)
	if err != nil {
		t.Fatalf("ApplyFixesWithApproval: %v", err)
	}
	if !strings.Contains(out, "applied-unresolved") {
		t.Errorf("want partial (applied-unresolved) summary, got %q", out)
	}
	if !strings.Contains(out, "unhandled-error: applied") {
		t.Errorf("applied item missing per-item line: %q", out)
	}
	if !strings.Contains(out, "unhandled-error: error") {
		t.Errorf("error item missing per-item line: %q", out)
	}
}

// TestRunToolLoop_ApplyFixesEndToEnd: the model's apply_fixes call flows
// through the loop → batch approval → both fixes applied → per-item outcome
// string fed back to the model → final answer.
func TestRunToolLoop_ApplyFixesEndToEnd(t *testing.T) {
	doc := multiFixDoc()
	var calls int32
	hook := func(_ context.Context, d *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		calls++
		switch blockID {
		case "b1":
			return afterFix1Doc(), nil
		default:
			return afterFix2Doc(), nil
		}
	}
	svc, _ := newFixTestService(doc, hook)

	rec := &fixEventRecorder{}
	ap := svc.newFixApplier("user-1", doc, "stream-1", rec.emit, nil, nil)
	tctx := ai.ToolContext{Ctx: context.Background(), Doc: doc, RealDoc: doc, Analysis: svc.analysisCache, Fixes: ap}

	prov := &recordingProvider{FakeProvider: testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "apply_fixes", Input: []byte(`{"targets":[{"blockId":"b1","ruleId":"unhandled-error"},{"blockId":"b2","ruleId":"unhandled-error"}]}`)}}, TokensIn: 10, TokensOut: 5},
		{Text: "Both fixed.", TokensIn: 8, TokensOut: 3},
	}}}

	ctl := &streamCtl{cancel: func() {}, started: make(chan struct{})}
	close(ctl.started)
	go func() {
		for i := 0; i < 5000; i++ {
			approved := false
			svc.pendingFixDecisions.Range(func(k, v any) bool {
				st := v.(*fixDecisionState)
				if st.streamID == "stream-1" {
					_ = svc.ResolveFixDecision("stream-1", k.(string), true, nil)
					approved = true
					return false
				}
				return true
			})
			if approved {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	emit, evs := collectEvents()
	svc.runToolLoop(context.Background(), prov, ai.Request{Messages: []ai.Message{{Role: "user", Content: "fix both"}}},
		"user-1", &tctx, ctl, func() bool { return true }, emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("stream did not complete; errMsg=%q", ctl.errMsg)
	}
	if calls != 2 {
		t.Fatalf("expected 2 applies, got %d", calls)
	}
	prov.mu.Lock()
	toolResults := ""
	if len(prov.reqs) > 1 {
		for _, m := range prov.reqs[1] {
			if m.Role == "tool" {
				toolResults += m.Content
			}
		}
	}
	prov.mu.Unlock()
	if !strings.Contains(toolResults, "Batch result") {
		t.Errorf("model's second turn missing batch outcome: %q", toolResults)
	}
	if got := strings.Join(*evs, ","); got != "tool,tool_result,chunk,done" {
		t.Errorf("expected tool,tool_result,chunk,done loop events, got %q", got)
	}
	fixes := rec.snapshot()
	if len(fixes) != 3 || fixes[0] != "fix_proposal" || fixes[1] != "fix_decision:applying" || fixes[2] != "fix_decision:applied" {
		t.Errorf("fix event sequence wrong: %v", fixes)
	}
}

func TestChatFixBatch_PerItemOptOut(t *testing.T) {
	// U4.1: approving a batch with deselected items applies ONLY the kept
	// ones; deselecting everything declines.
	doc := multiFixDoc()
	svc, _ := newFixTestService(doc, nil)
	svc.fixDecisionWait = 5 * time.Second
	rec := &batchEventRecorder{}
	ap := svc.newFixApplier("user-1", doc, "stream-1", rec.emit, nil, nil)

	props := []ai.FixProposal{
		{ProposalID: "p1", RuleID: "unhandled-error", FixType: "wrap-error-handler", BlockID: "b1", BlockLabel: "Call API", Fingerprint: realFingerprintFor(t, svc, doc, "unhandled-error", "b1")},
		{ProposalID: "p2", RuleID: "unhandled-error", FixType: "wrap-error-handler", BlockID: "b2", BlockLabel: "Sync API", Fingerprint: realFingerprintFor(t, svc, doc, "unhandled-error", "b2")},
	}
	go func() {
		for i := 0; i < 2500; i++ {
			resolved := false
			svc.pendingFixDecisions.Range(func(k, v any) bool {
				st := v.(*fixDecisionState)
				if st.streamID == "stream-1" {
					// Deselect the SECOND item (index 1).
					_ = svc.ResolveFixDecision("stream-1", k.(string), true, []int{1})
					resolved = true
					return false
				}
				return true
			})
			if resolved {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	out, err := ap.ApplyFixesWithApproval(context.Background(), props)
	if err != nil {
		t.Fatalf("ApplyFixesWithApproval: %v", err)
	}
	if strings.Contains(out, "1 of 2") || strings.Contains(out, "2 of 2") {
		t.Errorf("expected only 1 fix applied, got summary %q", out)
	}
	if !strings.Contains(out, "unhandled-error: applied") {
		t.Errorf("kept fix not applied: %q", out)
	}
}

func TestChatFixBatch_AllDeselectedDeclines(t *testing.T) {
	doc := multiFixDoc()
	svc, _ := newFixTestService(doc, nil)
	svc.fixDecisionWait = 5 * time.Second
	rec := &batchEventRecorder{}
	ap := svc.newFixApplier("user-1", doc, "stream-1", rec.emit, nil, nil)

	props := []ai.FixProposal{
		{ProposalID: "p1", RuleID: "unhandled-error", FixType: "wrap-error-handler", BlockID: "b1", BlockLabel: "Call API", Fingerprint: realFingerprintFor(t, svc, doc, "unhandled-error", "b1")},
	}
	go func() {
		for i := 0; i < 2500; i++ {
			resolved := false
			svc.pendingFixDecisions.Range(func(k, v any) bool {
				st := v.(*fixDecisionState)
				if st.streamID == "stream-1" {
					_ = svc.ResolveFixDecision("stream-1", k.(string), true, []int{0})
					resolved = true
					return false
				}
				return true
			})
			if resolved {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	out, err := ap.ApplyFixesWithApproval(context.Background(), props)
	if err != nil {
		t.Fatalf("ApplyFixesWithApproval: %v", err)
	}
	if !strings.Contains(out, "deselected every fix") {
		t.Errorf("want deselected-everything decline, got %q", out)
	}
}
