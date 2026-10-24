package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// failingScrubDocument makes scrubDocument fail from its from-th call on (1 = every call), restored when the test
// ends. ScrubDocument itself never fails today; the seam is how the fail-closed paths are reached.
func failingScrubDocument(t *testing.T, from int) {
	t.Helper()
	prev := scrubDocument
	calls := 0
	scrubDocument = func(doc *models.FlowDocument) (*models.FlowDocument, error) {
		calls++
		if calls >= from {
			return nil, errors.New("scrub boom")
		}
		return prev(doc)
	}
	t.Cleanup(func() { scrubDocument = prev })
}

// TestChatContext_ScrubFailureFailsClosed (R-testing-c15): when the flow's redacting clone cannot be made, the
// preview and the turn fail — the document itself never stands in for its clone, and nothing reaches the provider.
func TestChatContext_ScrubFailureFailsClosed(t *testing.T) {
	doc := parseCtxBlockDoc(t, ctxBlockSrc)
	p := &testutil.FakeProvider{RequireScrubbed: t, Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 3, TokensOut: 2}}}
	svc := newCtxBlockChatService(t, "scrub-fail", p)
	failingScrubDocument(t, 1)

	req := models.ChatRequest{Provider: "scrub-fail", Model: "fake-model", UserMessage: "what does this flow do?"}
	if res, err := svc.PreviewContext(context.Background(), "u1", doc, nil, req); !errors.Is(err, errContextScrub) || res != nil {
		t.Errorf("PreviewContext = %+v, %v; want errContextScrub and no preview", res, err)
	}

	req.ClientStreamID = uuid.NewString()
	id, err := svc.StreamChatMessage(context.Background(), "u1", doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	waitStreamFinished(t, svc, id)
	types, payloads := eventTypes(svc.notifier.(*testutil.CountingNotifier))
	failed := false
	for i, typ := range types {
		if typ == "error" {
			failed = true
			if payloads[i]["code"] != ChatErrContextUnavailable {
				t.Errorf("error event = %v, want code %s", payloads[i], ChatErrContextUnavailable)
			}
		}
	}
	if !failed {
		t.Errorf("the turn did not fail: events %v", types)
	}
	if n := len(p.Requests()); n != 0 {
		t.Errorf("%d provider requests were sent after the scrub failed, want none", n)
	}
}

// TestRunToolLoop_PostFixScrubFailureKeepsTheRedactedClone (R-testing-c15): an approved apply hands the fresh
// document to the loop; when its redacting clone cannot be made the tools keep the previous clone — never the
// fresh document itself — and the apply result tells the model so.
func TestRunToolLoop_PostFixScrubFailureKeepsTheRedactedClone(t *testing.T) {
	doc0, doc1, _, doc3 := threeFixDocs()
	hook := func(_ context.Context, _ *models.FlowDocument, blockID, _, _, _, _ string) (*models.FlowDocument, error) {
		if blockID == "b1" {
			return doc1, nil
		}
		return doc3, nil
	}
	svc, _ := newFixTestService(doc0, hook)
	ctl := startedCtl()
	ev := &countingEvents{}

	tctx := &ai.ToolContext{Ctx: context.Background(), Doc: doc0, RealDoc: doc0, Analysis: readOnlyAnalysis{svc.analysisCache}}
	tctx.Fixes = svc.newFixApplier(doc0, "stream-1", ev.emit, nil, toolDocSwap(tctx))
	failingScrubDocument(t, 1) // the loop was handed its clone; the post-fix clone is the first scrub asked for

	stop := make(chan struct{})
	defer close(stop)
	go approveAll(svc, "stream-1", stop)

	call := func(id, name, input string) testutil.FakeTurn {
		return testutil.FakeTurn{ToolCalls: []ai.ToolCall{{ID: id, Name: name, Input: []byte(input)}}, TokensIn: 1, TokensOut: 1}
	}
	stub := &testutil.FakeProvider{Tools: true, RequireScrubbed: t, Turns: []testutil.FakeTurn{
		call("a1", "apply_fix", `{"blockId":"b1","ruleId":"unhandled-error"}`),
		call("g1", "get_block", `{"blockId":"x2"}`), // an id the re-parse minted: only the fresh document has it
		{Text: "Done.", TokensIn: 1, TokensOut: 1},
	}}
	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "fix the first one"}}},
		"user-1", tctx, ctl, func() bool { return true }, ev.emit, func() int { return 0 })
	if !ctl.done {
		t.Fatalf("the turn did not finish: %s %s", ctl.errCode, ctl.errMsg)
	}

	if tctx.RealDoc != doc1 {
		t.Errorf("the applier's real document was not swapped to the fixed flow")
	}
	if tctx.Doc != doc0 {
		t.Errorf("the tools' document changed although the fresh clone could not be made (fresh itself: %v)", tctx.Doc == doc1)
	}
	reqs := stub.Requests()
	if len(reqs) < 3 {
		t.Fatalf("only %d provider turns", len(reqs))
	}
	applyResult := reqs[1].Messages[len(reqs[1].Messages)-1].Content
	if !strings.Contains(applyResult, "could not be prepared") || !strings.Contains(applyResult, "start a new turn") {
		t.Errorf("the apply result does not tell the model the tools are stale:\n%s", applyResult)
	}
	// The read tools answer from the previous clone: the fresh document's ids are unknown to them.
	read := reqs[2].Messages[len(reqs[2].Messages)-1].Content
	if !strings.Contains(read, `block "x2" not found`) {
		t.Errorf("get_block after the failed clone read the fresh document:\n%s", read)
	}
}

// contextCoreForTest is cachedContextCore for a document whose clone can be made (every fixture here); the
// fail-closed path has its own test (chat_scrub_failclosed_test.go).
func (s *ChatService) contextCoreForTest(ctx context.Context, scope string, provider ai.Provider, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) chatContextValue {
	cv, err := s.cachedContextCore(ctx, scope, provider, doc, report, req)
	if err != nil {
		panic(err)
	}
	return cv
}

// computeContextCoreForTest is computeContextCore for a document whose clone can be made.
func (s *ChatService) computeContextCoreForTest(provider ai.Provider, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) chatContextValue {
	cv, err := s.computeContextCore(provider, doc, report, req)
	if err != nil {
		panic(err)
	}
	return cv
}
