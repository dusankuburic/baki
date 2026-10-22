package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/ai"
)

// TestLoopToolMemory_ReadsRunAgainAfterTheDocumentIsReplaced pins R-tool-loop-1: an applied fix re-parses the
// flow and swaps the tool context's document (onDocReplaced). Every cached read was computed against the old
// document, so the same call must run again — and without an apply in between, the repetition guard still
// answers from the cache.
func TestLoopToolMemory_ReadsRunAgainAfterTheDocumentIsReplaced(t *testing.T) {
	doc0, doc1, _, _ := threeFixDocs()
	svc, _ := newFixTestService(doc0, nil)
	tctx := &ai.ToolContext{Ctx: context.Background(), Doc: doc0, RealDoc: doc0, Analysis: readOnlyAnalysis{svc.analysisCache}}
	mem := newLoopToolMemory()

	first := mem.exec("list_findings", []byte(`{}`), tctx)
	if !strings.Contains(first, "block=b1") {
		t.Fatalf("fixture: the first listing does not name b1:\n%s", first)
	}
	if again := mem.exec("list_findings", []byte(`{}`), tctx); !strings.Contains(again, "already answered") {
		t.Errorf("without an apply the identical call must still be answered from the cache: %q", again)
	}

	tctx.RealDoc, tctx.Doc = doc1, doc1 // what onDocReplaced does after an approved apply
	fresh := mem.exec("list_findings", []byte(`{}`), tctx)
	if strings.Contains(fresh, "already answered") {
		t.Errorf("the listing after an apply was served from the pre-fix cache:\n%s", fresh)
	}
	if !strings.Contains(fresh, "block=x2") || strings.Contains(fresh, "block=b1") {
		t.Errorf("the listing after an apply does not reflect the re-parsed document:\n%s", fresh)
	}
}

// TestLoopToolMemory_ErrorsAndSentinelsAreNotCached: a transient "error:" result or the "analysis not
// available" sentinel used to be pinned for the whole run, so a retry got the failure back plus a note telling
// the model to change tactic.
func TestLoopToolMemory_ErrorsAndSentinelsAreNotCached(t *testing.T) {
	mem := newLoopToolMemory()
	noAnalysis := &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}
	for _, call := range []struct{ name, input string }{
		{"list_findings", `{}`},      // "analysis not available"
		{"no_such_tool", `{}`},       // "error: unknown tool"
		{"get_block", `{"blockId":`}, // "error: … not valid JSON" for arguments cut mid-call
	} {
		_ = mem.exec(call.name, []byte(call.input), noAnalysis)
		if got := mem.exec(call.name, []byte(call.input), noAnalysis); strings.Contains(got, "already answered") {
			t.Errorf("%s: a failed result was served from the cache on retry: %q", call.name, got)
		}
	}
}

// decliningApplier answers every approval with the applier's declined result.
type decliningApplier struct{ calls atomic.Int32 }

func (d *decliningApplier) ApplyFixWithApproval(context.Context, ai.FixProposal) (string, error) {
	d.calls.Add(1)
	return fixDeclinedMarker + " this fix — nothing was changed.", nil
}

func (d *decliningApplier) ApplyFixesWithApproval(context.Context, []ai.FixProposal) (string, error) {
	d.calls.Add(1)
	return fixDeclinedMarker + " these fixes — nothing was changed.", nil
}

// TestLoopToolMemory_DeclineSurvivesTheDocumentSwap: a declined fix stays refused after another fix re-parses
// the flow, although its block now has a new ID and has moved down. Keyed by raw arguments, the re-request
// with the new ID asked the user again.
func TestLoopToolMemory_DeclineSurvivesTheDocumentSwap(t *testing.T) {
	doc0, _, _, _ := threeFixDocs()
	svc, _ := newFixTestService(doc0, nil)
	applier := &decliningApplier{}
	tctx := &ai.ToolContext{Ctx: context.Background(), Doc: doc0, RealDoc: doc0, Analysis: readOnlyAnalysis{svc.analysisCache}, Fixes: applier}
	mem := newLoopToolMemory()

	if got := mem.exec("apply_fix", []byte(`{"blockId":"b2","ruleId":"unhandled-error"}`), tctx); !strings.Contains(got, fixDeclinedMarker) {
		t.Fatalf("fixture: the first request was not declined: %q", got)
	}

	// Another fix lands: b2 is re-parsed as x2, three lines lower.
	_, doc1, _, _ := threeFixDocs()
	tctx.RealDoc, tctx.Doc = doc1, doc1
	got := mem.exec("apply_fix", []byte(`{"blockId":"x2","ruleId":"unhandled-error"}`), tctx)
	if !strings.HasPrefix(got, "error:") || !strings.Contains(got, "DECLINED") {
		t.Errorf("the declined fix was not refused under its new block ID: %q", got)
	}
	if n := applier.calls.Load(); n != 1 {
		t.Errorf("the user was asked %d times about a fix they already declined", n)
	}

	// A different finding is not caught by the decline.
	if got := mem.exec("apply_fix", []byte(`{"blockId":"x3","ruleId":"unhandled-error"}`), tctx); strings.HasPrefix(got, "error:") && strings.Contains(got, "already DECLINED") {
		t.Errorf("a fix the user never saw was refused as declined: %q", got)
	}
}
