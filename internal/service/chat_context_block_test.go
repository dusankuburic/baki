package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
	"pad-core/parser"
)

// ctxBlockSrc has two blocks of one type and name, so the anchor must also tell them apart by line.
const ctxBlockSrc = "#Region \"Main\"\n" +
	"    HTTPClient.InvokeUrl Url: $'''https://first.example''' Method: HTTPClient.Method.GET\n" +
	"    HTTPClient.InvokeUrl Url: $'''https://second.example''' Method: HTTPClient.Method.GET\n" +
	"#EndRegion\n"

// ctxBlockSrcFixed is the same flow after an edit added a line above both blocks: a re-parse, so new ids.
const ctxBlockSrcFixed = "#Region \"Main\"\n" +
	"    # checked\n" +
	"    HTTPClient.InvokeUrl Url: $'''https://first.example''' Method: HTTPClient.Method.GET\n" +
	"    HTTPClient.InvokeUrl Url: $'''https://second.example''' Method: HTTPClient.Method.GET\n" +
	"#EndRegion\n"

func parseCtxBlockDoc(t *testing.T, src string) *models.FlowDocument {
	t.Helper()
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	doc.ID = "f1"
	doc.RebuildIndexes()
	return doc
}

// blockWithURL is the id of the block whose Url property holds url.
func blockWithURL(t *testing.T, doc *models.FlowDocument, url string) string {
	t.Helper()
	for id, b := range doc.BlocksByID {
		if strings.Contains(b.Properties["Url"], url) {
			return id
		}
	}
	t.Fatalf("no block with url %q", url)
	return ""
}

func newCtxBlockChatService(t *testing.T, providerID string, p ai.Provider) *ChatService {
	t.Helper()
	ai.RegisterProvider(providerID, func(string) ai.Provider { return p })
	return &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
	}
}

// TestPreviewContext_ReanchorsAStaleContextBlock pins R-analyzer-boundary-2: a thread's context block is a
// parser-minted id, and every applied fix re-parses the flow and mints new ones. The next turn looked the stale
// id up, found nothing and silently left the "Selected Block" section out. The block is now found again by its
// content anchor.
func TestPreviewContext_ReanchorsAStaleContextBlock(t *testing.T) {
	before := parseCtxBlockDoc(t, ctxBlockSrc)
	staleID := blockWithURL(t, before, "second.example")
	after := parseCtxBlockDoc(t, ctxBlockSrcFixed)
	wantID := blockWithURL(t, after, "second.example")
	if _, ok := after.BlocksByID[staleID]; ok {
		t.Fatal("fixture: the re-parse kept the block id")
	}
	svc := newCtxBlockChatService(t, "ctxblock-preview", &testutil.FakeProvider{})

	res, err := svc.PreviewContext(context.Background(), "u1", after, nil, models.ChatRequest{
		Provider: "ctxblock-preview", Model: "fake-model", UserMessage: "what does this block do?",
		ContextBlockID: staleID, ContextBlockAnchor: before.AnchorOf(staleID),
	})
	if err != nil {
		t.Fatalf("PreviewContext: %v", err)
	}
	section := res.ContextText[strings.Index(res.ContextText, "## Selected Block")+1:]
	if !strings.Contains(res.ContextText, "## Selected Block") || !strings.Contains(section, "second.example") {
		t.Fatalf("the context lost the selected block (or found the wrong one):\n%s", res.ContextText)
	}
	if res.ContextBlock != "resolved" || res.ContextBlockID != wantID {
		t.Errorf("preview reports contextBlock=%q id=%q, want resolved to %q", res.ContextBlock, res.ContextBlockID, wantID)
	}
}

// TestPreviewContext_MissingContextBlockIsReported: a context block that cannot be found again is said so, to the
// caller and to the model, never a silently missing section.
func TestPreviewContext_MissingContextBlockIsReported(t *testing.T) {
	after := parseCtxBlockDoc(t, ctxBlockSrcFixed)
	svc := newCtxBlockChatService(t, "ctxblock-missing", &testutil.FakeProvider{})

	res, err := svc.PreviewContext(context.Background(), "u1", after, nil, models.ChatRequest{
		Provider: "ctxblock-missing", Model: "fake-model", UserMessage: "what does this block do?",
		ContextBlockID: "gone", ContextBlockAnchor: &models.BlockAnchor{Subflow: "Main", RawType: "Web.Removed", Name: "Removed", Count: 1},
	})
	if err != nil {
		t.Fatalf("PreviewContext: %v", err)
	}
	if res.ContextBlock != "missing" {
		t.Errorf("preview reports contextBlock=%q, want missing", res.ContextBlock)
	}
	if !strings.Contains(res.ContextText, "no longer in the flow") {
		t.Errorf("the model is not told the block is gone:\n%s", res.ContextText)
	}
}

// TestStreamChatMessage_MissingContextBlockEmitsANotice: on a turn, the missing block is a visible row in the
// stream, and a re-anchored one reaches the model.
func TestStreamChatMessage_MissingContextBlockEmitsANotice(t *testing.T) {
	before := parseCtxBlockDoc(t, ctxBlockSrc)
	staleID := blockWithURL(t, before, "second.example")
	after := parseCtxBlockDoc(t, ctxBlockSrcFixed)

	for _, tc := range []struct {
		name        string
		anchor      *models.BlockAnchor
		wantNotice  bool
		wantInModel string
	}{
		{"re-anchored", before.AnchorOf(staleID), false, "second.example"},
		{"missing", nil, true, "no longer in the flow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &testutil.FakeProvider{RequireScrubbed: t, Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 3, TokensOut: 2}}}
			svc := newCtxBlockChatService(t, "ctxblock-stream-"+tc.name, p)
			id, err := svc.StreamChatMessage(context.Background(), "u1", after, nil, models.ChatRequest{
				Provider: "ctxblock-stream-" + tc.name, Model: "fake-model", UserMessage: "what does this block do?",
				ContextBlockID: staleID, ContextBlockAnchor: tc.anchor, ClientStreamID: uuid.NewString(),
			})
			if err != nil {
				t.Fatalf("StreamChatMessage: %v", err)
			}
			waitStreamFinished(t, svc, id)

			types, payloads := eventTypes(svc.notifier.(*testutil.CountingNotifier))
			notices := 0
			for i, typ := range types {
				if typ == "tool" && payloads[i]["name"] == "context_block_missing" {
					notices++
				}
			}
			if tc.wantNotice != (notices == 1) || notices > 1 {
				t.Errorf("context_block_missing rows = %d (events %v), want notice=%v", notices, types, tc.wantNotice)
			}
			reqs := p.Requests()
			if len(reqs) == 0 {
				t.Fatal("no provider request")
			}
			last := reqs[0].Messages[len(reqs[0].Messages)-1].Content
			if !strings.Contains(last, tc.wantInModel) {
				t.Errorf("the model's turn lacks %q:\n%s", tc.wantInModel, last)
			}
		})
	}
}

// TestApplyWithApproval_FollowsTheGroundedBlock: an approved apply re-parses the flow and the model is told "ALL
// block IDs have changed"; the result now also says where the conversation's own block went.
func TestApplyWithApproval_FollowsTheGroundedBlock(t *testing.T) {
	doc := fixTestDoc() // b1 "Call API", line 3
	wrapped := &models.FlowDocument{ID: doc.ID, Name: doc.Name}
	wrapped.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main", Blocks: []models.Block{
		{ID: "fresh-handler", Name: "On Block Error", Type: models.BlockTypeErrorHandler, RawType: "ON_BLOCK_ERROR", LineNumber: 3},
		{ID: "fresh-b1", Name: "Call API", Type: models.BlockTypeAction, RawType: "HTTPClient.InvokeUrl", LineNumber: 4, Properties: map[string]string{"Url": "https://x"}},
	}}}
	wrapped.RebuildIndexes()
	gone := &models.FlowDocument{ID: doc.ID, Name: doc.Name}
	gone.Subflows = []models.Subflow{{ID: "sf-main", Name: "Main"}}
	gone.RebuildIndexes()

	for _, tc := range []struct {
		name  string
		fresh *models.FlowDocument
		want  string
	}{
		{"moved", wrapped, "now block id fresh-b1"},
		{"removed", gone, "no longer in the flow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newFixTestService(doc, func(context.Context, *models.FlowDocument, string, string, string, string, string) (*models.FlowDocument, error) {
				return tc.fresh, nil
			})
			rec := &fixEventRecorder{}
			ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil).(*chatFixApplier)
			ap.groundOn(doc, "b1")
			prop := fixTestProposal()
			prop.Fingerprint = realFingerprintFor(t, svc, doc, prop.RuleID, prop.BlockID)
			go func() {
				for range 1000 {
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
			if !strings.Contains(out, tc.want) {
				t.Errorf("the apply result does not say where the grounded block went (want %q):\n%s", tc.want, out)
			}
		})
	}
}
