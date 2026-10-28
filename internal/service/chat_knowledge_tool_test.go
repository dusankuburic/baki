package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// kbToolSecret is a value the text scrubber masks (a `password:` value) planted in the knowledge base's chunks.
const kbToolSecret = "Qz7vLm2pXk9rTw4n"

// kbToolStore is org-A's knowledge base for the tool tests: five long chunks under identity, each naming the
// service-account rotation and carrying kbToolSecret, so one page is well over the token cap.
func kbToolStore(identity string) *testutil.FakeBackend {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-A")
	_ = store.SaveKnowledgeDocument(context.Background(), "", &interfaces.KnowledgeDocument{ID: "kb-doc", OrgID: "org-A", Filename: "accounts.md"})
	var chunks []interfaces.KnowledgeChunk
	for i := 0; i < 5; i++ {
		chunks = append(chunks, interfaces.KnowledgeChunk{
			ID: "kb-" + string(rune('a'+i)), DocID: "kb-doc", Embedding: []float32{1}, EmbeddingIdentity: identity,
			Content: "Service accounts rotate quarterly; the staging password: " + kbToolSecret + " is issued by Ops. " + strings.Repeat("Rotation steps are logged in the change calendar and reviewed by the platform team. ", 30),
		})
	}
	_ = store.SaveKnowledgeChunks(context.Background(), "", chunks)
	return store
}

// kbToolService is a chat service on org-A (u1 is a member of every org: turnReadsBackend) whose provider id runs
// p, with knowledge as its knowledge base (nil: none wired).
func kbToolService(t *testing.T, providerID string, p *testutil.FakeProvider, knowledge *rag.KnowledgeService) *ChatService {
	t.Helper()
	ai.RegisterProvider(providerID, func(string) ai.Provider { return p })
	return &ChatService{
		notifier: &testutil.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory:  ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil),
		settings: nilSettings{}, backend: &turnReadsBackend{FakeBackend: testutil.NewFakeBackend()}, chatCtxCache: newChatContextCache(),
		knowledge: knowledge,
	}
}

// kbToolTurn runs one tool turn of u1 on doc and returns the stream's events.
func kbToolTurn(t *testing.T, svc *ChatService, providerID string, doc *models.FlowDocument) []map[string]interface{} {
	t.Helper()
	id, err := svc.StreamChatMessage(asUser("u1"), "u1", doc, nil, models.ChatRequest{
		Provider: providerID, Model: "fake-model", UserMessage: "how do we rotate the staging service account?", UseTools: true, ClientStreamID: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	waitStreamFinished(t, svc, id)
	var out []map[string]interface{}
	for _, ev := range svc.notifier.(*testutil.CountingNotifier).Events() {
		if env, ok := ev.Data.(map[string]interface{}); ok && ev.Name == "chat:event" {
			out = append(out, env)
		}
	}
	return out
}

func offered(req ai.Request, tool string) bool {
	for _, td := range req.Tools {
		if td.Name == tool {
			return true
		}
	}
	return false
}

// toolMessage is the result of the tool call id as the next request carries it.
func toolMessage(req ai.Request, id string) string {
	for _, m := range req.Messages {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

func searchKnowledgeCall(id, query string) ai.ToolCall {
	in, _ := json.Marshal(map[string]string{"query": query})
	return ai.ToolCall{ID: id, Name: "search_knowledge", Input: in}
}

// R-knowledge-6: a model that learned mid-loop what matters asks the organisation's knowledge base itself. The
// tool is offered and named in the system prompt; its result is the guidelines page, scrubbed and cut to the
// turn's guidelines cap, and the lookup is counted as the tool's.
func TestKnowledgeTool_ModelSearchesMidLoop(t *testing.T) {
	p := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{searchKnowledgeCall("k1", "service account rotation policy")}, TokensIn: 1, TokensOut: 1},
		{Text: "Rotate it quarterly.", TokensIn: 1, TokensOut: 1},
	}}
	svc := kbToolService(t, "kbtool-midloop", p, rag.NewKnowledgeService(kbToolStore("A"), nil, nil, rag.WithEmbedder(&lookupEmbedder{identity: "A", vec: []float32{1}})))
	before := promtestRAG("tool", "hit")
	kbToolTurn(t, svc, "kbtool-midloop", &models.FlowDocument{ID: "f-kb", Name: "f-kb", OrganizationID: "org-A"})

	reqs := p.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(reqs))
	}
	if !offered(reqs[0], "search_knowledge") || !strings.Contains(reqs[0].SystemPrompt, "search_knowledge") {
		t.Errorf("the org turn was not offered search_knowledge or not told of it (tools %v)", reqs[0].Tools)
	}
	res := toolMessage(reqs[1], "k1")
	if !strings.Contains(res, "accounts.md") || !strings.Contains(res, "rotate quarterly") {
		t.Fatalf("the tool result does not carry the guidelines page: %q", res)
	}
	if strings.Contains(res, kbToolSecret) {
		t.Errorf("the tool result carries the chunk's secret: %q", res)
	}
	if n := ai.EstimateTokens(res); n > ragGuidelinesTokenCap+20 {
		t.Errorf("the tool result is %d tokens, over the guidelines cap %d", n, ragGuidelinesTokenCap)
	}
	if d := promtestRAG("tool", "hit") - before; d != 1 {
		t.Errorf("pad_chat_rag_lookups_total{source=tool,outcome=hit} rose by %v, want 1", d)
	}
}

// The knowledge notice belongs to the turn's pre-turn lookup: a tool call that meets the same reason (here the
// member's model cannot see the corpus) answers the model, and the stream still says it once.
func TestKnowledgeTool_NoNoticeFromTheTool(t *testing.T) {
	p := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{searchKnowledgeCall("k1", "service account rotation policy")}, TokensIn: 1, TokensOut: 1},
		{Text: "I could not read the guidelines.", TokensIn: 1, TokensOut: 1},
	}}
	svc := kbToolService(t, "kbtool-mismatch", p, rag.NewKnowledgeService(kbToolStore("B"), nil, nil, rag.WithEmbedder(&lookupEmbedder{identity: "A", vec: []float32{1}})))
	before := promtestRAG("tool", "identity_mismatch")
	events := kbToolTurn(t, svc, "kbtool-mismatch", &models.FlowDocument{ID: "f-kb", Name: "f-kb", OrganizationID: "org-A"})
	notices := 0
	for _, env := range events {
		if data, _ := env["data"].(map[string]interface{}); env["type"] == "notice" && data["kind"] == "knowledge" {
			notices++
		}
	}
	if notices != 1 {
		t.Errorf("knowledge notices = %d, want the pre-turn lookup's one", notices)
	}
	if res := toolMessage(p.Requests()[1], "k1"); !strings.HasPrefix(res, "error:") || !strings.Contains(res, "another embedding model") {
		t.Errorf("the tool result = %q, want the mismatch explained", res)
	}
	if d := promtestRAG("tool", "identity_mismatch") - before; d != 1 {
		t.Errorf("the tool's identity_mismatch rose by %v, want 1", d)
	}
}

// A caller outside the flow's organisation (a collaborator on an org flow) gets "not available" from the tool,
// counted as non_member, and nothing is embedded.
func TestKnowledgeTool_NonMemberIsRefused(t *testing.T) {
	emb := &lookupEmbedder{identity: "A", vec: []float32{1}}
	svc := lookupService(rag.NewKnowledgeService(kbToolStore("A"), nil, nil, rag.WithEmbedder(emb)))
	tool := svc.knowledgeTool("user-2", &models.FlowDocument{ID: "f1", OrganizationID: "org-A"})
	before := promtestRAG("tool", "non_member")
	out := ai.ExecuteTool("search_knowledge", []byte(`{"query":"service account rotation"}`), ai.ToolContext{Ctx: context.Background(), Knowledge: tool})
	if !strings.HasPrefix(out, "error:") || !strings.Contains(out, "not available") {
		t.Errorf("a non-member's search = %q, want not available", out)
	}
	if d := promtestRAG("tool", "non_member") - before; d != 1 {
		t.Errorf("the tool's non_member rose by %v, want 1", d)
	}
	if n := len(emb.embedded()); n != 0 {
		t.Errorf("a non-member's search embedded %d queries", n)
	}
}

// The tool needs the flow's organisation and a knowledge service: the desktop and local mode (no knowledge
// service) and a personal flow are never offered it, nor told of it.
func TestKnowledgeTool_NotOfferedWithoutAKnowledgeBase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		knowledge *rag.KnowledgeService
		org       string
	}{
		{"no knowledge service", nil, "org-A"},
		{"personal flow", rag.NewKnowledgeService(kbToolStore("A"), nil, nil, rag.WithEmbedder(&lookupEmbedder{identity: "A", vec: []float32{1}})), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{{Text: "ok", TokensIn: 1, TokensOut: 1}}}
			id := "kbtool-none-" + strings.ReplaceAll(tc.name, " ", "-")
			svc := kbToolService(t, id, p, tc.knowledge)
			kbToolTurn(t, svc, id, &models.FlowDocument{ID: "f-kb", Name: "f-kb", OrganizationID: tc.org})
			reqs := p.Requests()
			if len(reqs) == 0 {
				t.Fatal("no provider request")
			}
			if offered(reqs[0], "search_knowledge") || strings.Contains(reqs[0].SystemPrompt, "search_knowledge") {
				t.Errorf("search_knowledge offered or mentioned without a knowledge base")
			}
			if !offered(reqs[0], "search_flow") {
				t.Error("the read tools were not offered")
			}
		})
	}
}

func promtestRAG(source, outcome string) float64 {
	return ragOutcomeCountFrom(source, outcome)
}

// A context preview looks the guidelines up as the turn would, but it is not a turn: it is counted under its own
// source, so pad_chat_rag_lookups_total{source="turn"} counts turns only.
func TestRAGLookup_PreviewHasItsOwnSource(t *testing.T) {
	p := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{{Text: "ok", TokensIn: 1, TokensOut: 1}}}
	svc := kbToolService(t, "kbtool-preview", p, rag.NewKnowledgeService(kbToolStore("A"), nil, nil, rag.WithEmbedder(&lookupEmbedder{identity: "A", vec: []float32{1}})))
	turnBefore, previewBefore := promtestRAG(ragSourceTurn, "hit"), promtestRAG(ragSourcePreview, "hit")
	res, err := svc.PreviewContext(asUser("u1"), "u1", &models.FlowDocument{ID: "f-kb", Name: "f-kb", OrganizationID: "org-A"}, nil, models.ChatRequest{
		Provider: "kbtool-preview", Model: "fake-model", UserMessage: "how do we rotate the staging service account?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.UserMessage, "accounts.md") {
		t.Fatalf("the preview has no guidelines: %q", res.UserMessage)
	}
	if d := promtestRAG(ragSourcePreview, "hit") - previewBefore; d != 1 {
		t.Errorf("{source=preview,outcome=hit} rose by %v, want 1", d)
	}
	if d := promtestRAG(ragSourceTurn, "hit") - turnBefore; d != 0 {
		t.Errorf("the preview counted %v turn lookups, want 0", d)
	}
}

// A knowledge base whose embedding provider does not answer: the search ends with its context's deadline (the
// tool's timeout in the loop, ai.ExecuteTool), the model reads why, and the lookup counts as the tool's deadline.
func TestKnowledgeTool_SlowEmbedderEndsAtTheDeadline(t *testing.T) {
	emb := &lookupEmbedder{identity: "A", block: true}
	svc := lookupService(rag.NewKnowledgeService(kbToolStore("A"), nil, nil, rag.WithEmbedder(emb)))
	tool := svc.knowledgeTool("user-1", &models.FlowDocument{ID: "f1", OrganizationID: "org-A"})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	before := promtestRAG(ragSourceTool, "deadline")
	start := time.Now()
	out := ai.ExecuteTool("search_knowledge", []byte(`{"query":"service account rotation"}`), ai.ToolContext{Ctx: ctx, Knowledge: tool})
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the search took %v under a 40ms deadline", took)
	}
	if !strings.HasPrefix(out, "error:") {
		t.Errorf("result %q, want an error the model can read", out)
	}
	if d := promtestRAG(ragSourceTool, "deadline") - before; d != 1 {
		t.Errorf("{source=tool,outcome=deadline} rose by %v, want 1", d)
	}
}
