package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
	"pad-core/parser"
)

// The context preview is meant to show what the model receives and whether it fits the window. It differed from
// the send in three ways (R-analyzer-boundary-3): the handler passed no report, so the Analysis Summary the
// turn writes first was never shown; the estimate counted the system prompt, the context and the question only,
// never the history or the tool schemas; and with a cached report the preview warmed another cache entry than
// the send. PreviewContext now builds the turn's own request (assembleTurnRequest) and reports its parts.

// parityFlowSrc has a hardcoded credential, a hardcoded URL and a loop without an exit, so the analysis has
// findings and the selected SET block has an output variable.
const parityFlowSrc = "#Region \"Main\"\n" +
	"    SET Password TO $'''hunter2-secret-value'''\n" +
	"    HTTPClient.InvokeUrl Url: $'''https://x.example/api?key=abc''' Method: HTTPClient.Method.GET Response=> Resp\n" +
	"    SET Total TO 0\n" +
	"    Display.ShowMessageDialog Title: $'''Hi''' Message: $'''%Total%'''\n" +
	"    LOOP WHILE (%Flag%) <> ($'''done''')\n" +
	"        Display.ShowMessageDialog Title: $'''Hi''' Message: $'''ok'''\n" +
	"    END\n" +
	"#EndRegion\n"

type parityFixture struct {
	svc      *ChatService
	provider *testutil.FakeProvider
	doc      *models.FlowDocument
	total    string // the SET Total block
}

func newParityFixture(t *testing.T, providerID string, native bool) *parityFixture {
	t.Helper()
	doc, err := parser.ParseText(parityFlowSrc, "Main.txt", int64(len(parityFlowSrc)))
	if err != nil {
		t.Fatal(err)
	}
	doc.ID = "parity-" + providerID
	doc.RebuildIndexes()
	analysis, err := NewAnalysisService(&testutil.CountingNotifier{}, newTestSettingsStore(t), nil, AnalysisOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Recorded, as the analyse-on-open path records it: CurrentReport answers from here on.
	if _, err := analysis.AnalyzeFlow(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := analysis.CurrentReport(doc); !ok {
		t.Fatal("fixture: no current report after AnalyzeFlow")
	}
	provider := &testutil.FakeProvider{Tools: native, RequireScrubbed: t, Turns: []testutil.FakeTurn{{Text: "answer", TokensIn: 3, TokensOut: 2}}}
	ai.RegisterProvider(providerID, func(string) ai.Provider { return provider })
	svc := &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: analysis,
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(),
	}
	total := ""
	for id, b := range doc.BlocksByID {
		if b.Properties["_output"] == "Total" {
			total = id
		}
	}
	if total == "" {
		t.Fatal("fixture: no SET Total block")
	}
	return &parityFixture{svc: svc, provider: provider, doc: doc, total: total}
}

func TestPreviewContext_ShowsTheCachedReportAsTheTurnDoes(t *testing.T) {
	fx := newParityFixture(t, "parity-report", true)
	res, err := fx.svc.PreviewContext(context.Background(), "u1", fx.doc, nil, models.ChatRequest{Provider: "parity-report", Model: "fake-model", UserMessage: "what is wrong?"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.ContextText, "## Analysis Summary") {
		t.Errorf("the preview lacks the Analysis Summary the turn sends first:\n%s", res.ContextText)
	}
}

func TestPreviewContext_CountsHistoryAndTools(t *testing.T) {
	fx := newParityFixture(t, "parity-count", true)
	history := []models.ChatMessage{
		{ID: "h1", Role: "user", Content: strings.Repeat("earlier question ", 40)},
		{ID: "h2", Role: "assistant", Content: strings.Repeat("earlier answer ", 40)},
	}
	req := models.ChatRequest{Provider: "parity-count", Model: "fake-model", UserMessage: "what is wrong?", Messages: history, UseTools: true}
	res, err := fx.svc.PreviewContext(context.Background(), "u1", fx.doc, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	est := fx.provider.EstimateTokens
	floor := est(res.SystemPrompt) + est(res.ContextText) + est(res.UserMessage)
	for _, m := range history {
		floor += est(m.Content)
	}
	for _, td := range ai.ToolDefinitionsFor(nil) {
		floor += est(string(td.InputSchema))
	}
	if res.EstimatedTokens < floor {
		t.Errorf("EstimatedTokens %d < %d, the system prompt + context + every history message + the tool schemas + the question", res.EstimatedTokens, floor)
	}
	if res.HistoryMessages != 2 {
		t.Errorf("HistoryMessages %d, want 2", res.HistoryMessages)
	}
	if !res.ToolsOffered || len(res.Tools) == 0 {
		t.Errorf("tools not reported as offered: %+v", res)
	}
	b := res.Breakdown
	if b.System <= 0 || b.Context <= 0 || b.History <= 0 || b.Tools <= 0 || b.User <= 0 {
		t.Errorf("breakdown has an empty part: %+v", b)
	}
	// The window check's own arithmetic (truncateForContextWindowMemo): the system prompt, each schema + 8, each
	// message + 4, the last message being the context frame and the rest of the turn.
	want := est(res.SystemPrompt)
	for _, td := range ai.ToolDefinitionsFor(nil) {
		want += est(string(td.InputSchema)) + 8
	}
	for _, m := range history {
		want += est(m.Content) + 4
	}
	want += est(ai.Frame(ai.FrameFlowContext, res.ContextText)+"\n\n"+res.UserMessage) + 4
	if res.EstimatedTokens != want {
		t.Errorf("EstimatedTokens %d, want the window check's %d", res.EstimatedTokens, want)
	}
	if sum := b.System + b.Context + b.History + b.Tools + b.User; sum != res.EstimatedTokens {
		t.Errorf("breakdown sums to %d, not the estimate %d", sum, res.EstimatedTokens)
	}
	if res.ContextBudget <= 0 {
		t.Errorf("no effective context budget reported: %+v", res)
	}
}

func TestPreviewContext_RebuildsTheStoredHistory(t *testing.T) {
	fx := newParityFixture(t, "parity-history", true)
	ctx := context.Background()
	req := models.ChatRequest{Provider: "parity-history", Model: "fake-model", UserMessage: "and now?", ConversationID: uuid.NewString()}
	stored := []models.ChatMessage{
		{ID: uuid.NewString(), Role: "user", Content: "first question"},
		{ID: uuid.NewString(), Role: "assistant", Content: "first answer"},
		{ID: uuid.NewString(), Role: "user", Content: "second question"},
		{ID: uuid.NewString(), Role: "assistant", Content: "second answer"},
	}
	if _, err := fx.svc.saveConversation(ctx, fx.doc.ID, req.ConversationKey(), stored); err != nil {
		t.Fatal(err)
	}
	res, err := fx.svc.PreviewContext(ctx, "u1", fx.doc, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.HistoryMessages != len(stored) {
		t.Errorf("HistoryMessages %d, want the %d stored (the send rebuilds the conversation when the request carries none)", res.HistoryMessages, len(stored))
	}
}

func TestPreviewContext_MarkerModeShowsTheToolInstructions(t *testing.T) {
	fx := newParityFixture(t, "parity-marker", false)
	res, err := fx.svc.PreviewContext(context.Background(), "u1", fx.doc, nil, models.ChatRequest{Provider: "parity-marker", Model: "fake-model", UserMessage: "q", UseTools: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SystemPrompt, "<tool_call>") {
		t.Errorf("the preview's system prompt lacks the marker-mode tool instructions the turn appends:\n%s", res.SystemPrompt)
	}
	if !res.ToolsOffered {
		t.Errorf("tools not reported as offered in marker mode: %+v", res)
	}
	if res.Breakdown.Tools != 0 {
		t.Errorf("marker mode sends no schemas, yet Tools = %d", res.Breakdown.Tools)
	}
}

func TestPreviewContext_SelectedBlockHasItsVariableHistory(t *testing.T) {
	fx := newParityFixture(t, "parity-lineage", true)
	res, err := fx.svc.PreviewContext(context.Background(), "u1", fx.doc, nil, models.ChatRequest{Provider: "parity-lineage", Model: "fake-model", UserMessage: "q", ContextBlockID: fx.total})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.ContextText, "## Variable History") || !strings.Contains(res.ContextText, "**%Total%**") {
		t.Errorf("the selected SET block's output variable has no Variable History:\n%s", res.ContextText)
	}
}

// The preview and the stream that follows it share one chat.context entry: the context is computed once.
func TestPreviewContext_SharesTheTurnsCacheEntry(t *testing.T) {
	fx := newParityFixture(t, "parity-cache", true)
	var computed atomic.Int32
	orig := contextScrub
	contextScrub = func(s string) string { computed.Add(1); return orig(s) }
	t.Cleanup(func() { contextScrub = orig })

	ctx := context.Background()
	req := models.ChatRequest{Provider: "parity-cache", Model: "fake-model", UserMessage: "what is wrong?", ContextBlockID: fx.total, ClientStreamID: uuid.NewString()}
	if _, err := fx.svc.PreviewContext(ctx, "u1", fx.doc, nil, req); err != nil {
		t.Fatal(err)
	}
	if n := computed.Load(); n != 2 { // the system prompt and the context text of one build
		t.Fatalf("the preview computed the context %d times (scrub calls), want one build", n/2)
	}
	id, err := fx.svc.StreamChatMessage(ctx, "u1", fx.doc, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	waitStreamFinished(t, fx.svc, id)
	if n := computed.Load(); n != 2 {
		t.Errorf("the stream after the preview rebuilt the context (%d scrub calls in total, want 2): the two use different cache keys", n)
	}
}
