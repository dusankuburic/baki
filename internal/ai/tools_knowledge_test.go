package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeKnowledge is a session's knowledge base for the tool tests: it records what it was asked and answers text or
// err.
type fakeKnowledge struct {
	text   string
	err    error
	query  string
	limit  int
	called int
}

func (k *fakeKnowledge) SearchKnowledge(_ context.Context, query string, limit int) (string, error) {
	k.called++
	k.query, k.limit = query, limit
	return k.text, k.err
}

// R-knowledge-6: search_knowledge is a read-only tool that needs a knowledge base wired into the session. The
// gates every caller already builds (NewToolGate: no knowledge base) and a nil gate never offer it; a session that
// wires one (ToolSession.Knowledge) does, and the turn's tool access then says so, which puts the one-line mention
// in the system prompt.
func TestSearchKnowledge_OfferedOnlyWithAKnowledgeBase(t *testing.T) {
	spec, ok := ToolSpecOf("search_knowledge")
	if !ok {
		t.Fatal("search_knowledge is not registered")
	}
	if spec.RequiresApproval || spec.RequiredScope != "" || spec.PreviewKind != "" || !spec.NeedsKnowledge {
		t.Errorf("spec = %+v; want a read-only tool that needs a knowledge base", spec)
	}
	has := func(defs []ToolDefinition) bool {
		for _, d := range defs {
			if d.Name == "search_knowledge" {
				return true
			}
		}
		return false
	}
	if has(ToolDefinitionsFor(nil)) {
		t.Error("a nil gate offers search_knowledge; it needs a session's knowledge base")
	}
	if got := ToolRefusal(spec, nil, true, true); got != ToolRefusedKnowledgeUnavailable {
		t.Errorf("ToolRefusal without a knowledge base = %q, want %q", got, ToolRefusedKnowledgeUnavailable)
	}
	if has(ToolDefinitionsFor(NewToolGate(nil, true, true))) {
		t.Error("NewToolGate offers search_knowledge")
	}
	gate := ToolSession{ApprovalsWired: true, CanEditFlow: true, Knowledge: true}.Gate()
	if !has(ToolDefinitionsFor(gate)) {
		t.Error("a session with a knowledge base is not offered search_knowledge")
	}
	if !strings.Contains(ToolPromptInstructionsFor(gate), "- search_knowledge:") {
		t.Error("the marker-mode instructions do not list search_knowledge")
	}
	if !has(ToolDefinitions()) {
		t.Error("ToolDefinitions (the metric's closed label set) lacks search_knowledge")
	}

	// The prompt names the tool only when it is offered.
	access := ToolAccessOf(true, gate)
	if access&ToolAccessKnowledge == 0 || access&^ToolAccessKnowledge != ToolAccessFixes {
		t.Errorf("ToolAccessOf = %v, want fixes with the knowledge flag", access)
	}
	if p := SystemPromptFor(access); !strings.Contains(p, "search_knowledge") || !strings.HasPrefix(p, systemPromptHead) {
		t.Errorf("the knowledge session's prompt does not mention search_knowledge:\n%s", p)
	}
	for _, a := range []ToolAccess{ToolAccessOf(true, NewToolGate(nil, true, true)), ToolAccessOf(true, nil), ToolAccessOf(false, gate)} {
		if strings.Contains(SystemPromptFor(a), "search_knowledge") {
			t.Errorf("access %v mentions search_knowledge", a)
		}
	}
	if SystemPromptFor(ToolAccessOf(true, nil)) != SystemPromptDefault {
		t.Error("the default prompt changed")
	}
	// Short: the prompt must leave room in small windows.
	if extra := EstimateTokens(SystemPromptFor(access)) - EstimateTokens(SystemPromptDefault); extra > 40 {
		t.Errorf("the knowledge mention costs %d tokens, want at most 40", extra)
	}
}

// search_knowledge hands the session's knowledge base the query and a limit of at most one page, and returns what
// it answers; a session without one, a missing query and a refusal are error results the model can read.
func TestSearchKnowledge_Execution(t *testing.T) {
	k := &fakeKnowledge{text: "- [policy.md] Retry SQL three times."}
	tctx := ToolContext{Ctx: context.Background(), Knowledge: k}
	out := ExecuteTool("search_knowledge", []byte(`{"query":"sql retry policy","limit":50}`), tctx)
	if out != k.text || k.query != "sql retry policy" || k.limit != MaxKnowledgeToolLimit {
		t.Errorf("result %q, asked %q limit %d; want the text, the query and the limit clamped to %d", out, k.query, k.limit, MaxKnowledgeToolLimit)
	}
	ExecuteTool("search_knowledge", []byte(`{"query":"sql"}`), tctx)
	if k.limit != MaxKnowledgeToolLimit {
		t.Errorf("default limit = %d, want %d", k.limit, MaxKnowledgeToolLimit)
	}
	ExecuteTool("search_knowledge", []byte(`{"query":"sql","limit":2}`), tctx)
	if k.limit != 2 {
		t.Errorf("limit = %d, want 2", k.limit)
	}
	if out := ExecuteTool("search_knowledge", []byte(`{"query":"  "}`), tctx); !strings.HasPrefix(out, "error:") {
		t.Errorf("an empty query = %q, want an error", out)
	}
	calls := k.called
	if out := ExecuteTool("search_knowledge", []byte(`{"query":"sql"}`), ToolContext{Ctx: context.Background()}); !strings.HasPrefix(out, "error:") || !strings.Contains(out, "not available") {
		t.Errorf("without a knowledge base = %q, want not available", out)
	}
	k.err = errors.New("the organisation's knowledge base is not available to you")
	if out := ExecuteTool("search_knowledge", []byte(`{"query":"sql"}`), tctx); out != "error: the organisation's knowledge base is not available to you" {
		t.Errorf("a refusal = %q", out)
	}
	if k.called != calls+1 {
		t.Errorf("knowledge asked %d times, want once more", k.called-calls)
	}
}

// blockingKnowledge waits for its context: a knowledge base whose embedding provider never answers.
type blockingKnowledge struct{ ctxErr error }

func (k *blockingKnowledge) SearchKnowledge(ctx context.Context, _ string, _ int) (string, error) {
	<-ctx.Done()
	k.ctxErr = ctx.Err()
	return "", ctx.Err()
}

// search_knowledge runs under its own timeout (the spec's), on the context the search itself is handed: a knowledge
// base that never answers ends the call at the tool's deadline, not at the stream's cap.
func TestSearchKnowledge_RunsUnderTheToolTimeout(t *testing.T) {
	spec := toolRegistry["search_knowledge"]
	if spec.Timeout <= 0 || spec.Timeout > 30*time.Second {
		t.Fatalf("search_knowledge timeout = %v, want its own bound", spec.Timeout)
	}
	defer func(was time.Duration) { spec.Timeout = was; toolRegistry["search_knowledge"] = spec }(spec.Timeout)
	short := spec
	short.Timeout = 30 * time.Millisecond
	toolRegistry["search_knowledge"] = short

	k := &blockingKnowledge{}
	start := time.Now()
	out := ExecuteTool("search_knowledge", []byte(`{"query":"sql retry"}`), ToolContext{Ctx: context.Background(), Knowledge: k})
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the call took %v under a 30ms tool timeout", took)
	}
	if !errors.Is(k.ctxErr, context.DeadlineExceeded) || !strings.Contains(out, "timed out") {
		t.Errorf("result %q, search context %v; want the tool's deadline on the search's own context", out, k.ctxErr)
	}
}
