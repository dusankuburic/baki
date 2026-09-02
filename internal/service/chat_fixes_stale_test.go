package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/ai/scrubber"
	"pad-core/models"
)

// TestAnalyzeFlowReadOnly_DoesNotOverwriteCurrentReport pins the A2 isolation:
// the chat tool loop analyzes its internal copies through AnalyzeFlowReadOnly,
// so the UI's CurrentReport / diff pair (same StableFlowID for scrubbed
// clones) never flips to the loop's transient report.
func TestAnalyzeFlowReadOnly_DoesNotOverwriteCurrentReport(t *testing.T) {
	doc := fixTestDoc()
	svc, _ := newFixTestService(doc, nil)

	// The recorded analysis the UI would show.
	recorded, err := svc.analysisCache.AnalyzeFlow(context.Background(), doc)
	if err != nil || recorded == nil {
		t.Fatalf("recorded analysis: %v", err)
	}

	// A different-content clone (as the scrubbed doc is) analyzed read-only.
	clone, cerr := scrubber.ScrubDocument(doc)
	if cerr != nil {
		t.Fatalf("scrub: %v", cerr)
	}
	clone.Subflows[0].Blocks[0].Properties = map[string]string{"Url": "https://changed"}
	ro, err := svc.analysisCache.AnalyzeFlowReadOnly(context.Background(), clone)
	if err != nil || ro == nil {
		t.Fatalf("read-only analysis: %v", err)
	}

	cur, ok := svc.analysisCache.CurrentReport(doc)
	if !ok || cur != recorded {
		t.Errorf("CurrentReport was overwritten by the read-only analysis (recorded=%p current=%p)", recorded, cur)
	}
}

// TestRunToolLoop_SecondFixAfterDocRefresh pins A1: after an approved apply,
// the loop's working documents are swapped to the fresh (re-parsed, NEW block
// IDs) document, so a second apply_fix targeting a fresh-doc block resolves
// instead of failing "no finding" against the stale pre-fix snapshot.
func TestRunToolLoop_SecondFixAfterDocRefresh(t *testing.T) {
	doc := fixTestDoc()

	// freshDocAfterFirstFix mirrors what FlowService.ApplyFix returns: same
	// flow, re-parsed — different block IDs — and still one fixable finding.
	fresh := &models.FlowDocument{ID: doc.ID, Name: doc.Name}
	fresh.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: []models.Block{
		{ID: "fresh-1", Name: "Sync API", Type: models.BlockTypeAction, RawType: "HTTPClient.InvokeUrl", LineNumber: 5, Properties: map[string]string{"Url": "https://other"}},
	}}}
	fresh.RebuildIndexes()
	clean := &models.FlowDocument{ID: doc.ID, Name: doc.Name}
	clean.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: []models.Block{
		{ID: "done-1", Name: "Comment", Type: models.BlockTypeAction, RawType: "Comment", LineNumber: 1},
	}}}
	clean.RebuildIndexes()

	var calls int32
	var mu sync.Mutex
	hook := func(_ context.Context, _ *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return fresh, nil
		}
		return clean, nil
	}
	svc, _ := newFixTestService(doc, hook)

	rec := &fixEventRecorder{}
	var tctx ai.ToolContext
	ap := svc.newFixApplier("user-1", doc, "stream-1", rec.emit, nil, func(freshDoc *models.FlowDocument) {
		tctx.RealDoc = freshDoc
		if sd, err := scrubber.ScrubDocument(freshDoc); err == nil {
			tctx.Doc = sd
		} else {
			tctx.Doc = freshDoc
		}
	})
	tctx = ai.ToolContext{Ctx: context.Background(), Doc: doc, RealDoc: doc, Analysis: readOnlyAnalysis{svc.analysisCache}, Fixes: ap}

	prov := &recordingProvider{FakeProvider: testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}}, TokensIn: 10, TokensOut: 5},
		{ToolCalls: []ai.ToolCall{{ID: "t2", Name: "apply_fix", Input: []byte(`{"blockId":"fresh-1","ruleId":"unhandled-error"}`)}}, TokensIn: 10, TokensOut: 5},
		{Text: "Both fixes applied.", TokensIn: 8, TokensOut: 3},
	}}}

	ctl := &streamCtl{cancel: func() {}, started: make(chan struct{})}
	close(ctl.started)

	// Approve every pending proposal on the stream as it lands.
	go func() {
		seen := map[string]bool{}
		for i := 0; i < 5000; i++ { // ~10s max
			svc.pendingFixDecisions.Range(func(k, v any) bool {
				st := v.(*fixDecisionState)
				if st.streamID == "stream-1" && !seen[k.(string)] {
					seen[k.(string)] = true
					_ = svc.ResolveFixDecision("stream-1", k.(string), true, nil)
				}
				return false
			})
			if len(seen) == 2 {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	emit, _ := collectEvents()
	svc.runToolLoop(context.Background(), prov, ai.Request{Messages: []ai.Message{{Role: "user", Content: "fix everything"}}},
		"user-1", &tctx, ctl, func() bool { return true }, emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("stream did not complete; errMsg=%q", ctl.errMsg)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("expected both apply_fix calls to reach the hook, got %d (errMsg=%q)", calls, ctl.errMsg)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	toolResults := ""
	if len(prov.reqs) >= 3 {
		for _, m := range prov.reqs[2] {
			if m.Role == "tool" {
				toolResults += m.Content
			}
		}
	}
	if !strings.Contains(toolResults, "APPLIED") {
		t.Errorf("second apply outcome missing from the model's turn: %q", toolResults)
	}
	fixes := rec.snapshot()
	if len(fixes) != 6 {
		t.Errorf("expected 6 fix events (proposal/decision ×2), got %v", fixes)
	}
}
