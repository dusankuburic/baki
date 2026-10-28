package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// agentToolsProvider streams a Foundry agent run whose own hosted tools ran: two web searches and an MCP call
// (start and completion each), then the answer. hosted=false streams the answer alone.
type agentToolsProvider struct {
	*testutil.FakeProvider
	hosted bool
}

func (p *agentToolsProvider) SupportsTools() bool { return true }

func (p *agentToolsProvider) Stream(_ context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
	if p.hosted {
		for _, name := range []string{"web_search_call", "mcp_call", "web_search_call"} {
			onChunk(ai.Chunk{ToolEvent: &ai.ToolEvent{Name: name, Label: "Agent · " + name}})
			onChunk(ai.Chunk{ToolEvent: &ai.ToolEvent{Name: name, Label: "Agent · " + name, Done: true, OK: true}})
		}
	}
	onChunk(ai.Chunk{Text: "answer"})
	onChunk(ai.Chunk{Done: true, TokensIn: 3, TokensOut: 2})
	return nil
}

// runAgentToolsTurn runs one agent turn on an org flow with an audit capture and returns the stream id and the
// rows recorded once the turn settled.
func runAgentToolsTurn(t *testing.T, hosted bool) (string, []*storageif.AuditEvent) {
	t.Helper()
	fake := &agentToolsProvider{FakeProvider: &testutil.FakeProvider{}, hosted: hosted}
	ai.RegisterProvider("agent-tools-fake", func(string) ai.Provider { return fake })
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	ch := make(chan *storageif.AuditEvent, 16)
	svc := &ChatService{
		notifier:      &testutil.CountingNotifier{},
		configDir:     t.TempDir(),
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
		settings:      nilSettings{},
		chatCtxCache:  newChatContextCache(),
		audit:         audit.NewCapture(ch),
	}
	doc := &models.FlowDocument{ID: "f1", Name: "f1", OrganizationID: "org-1"}
	ctx := auth.WithClaims(context.Background(), &auth.Claims{UserID: "u-owner", Email: "owner@acme.io"})
	id, err := svc.StreamChatMessage(ctx, "u-owner", doc, nil, models.ChatRequest{
		Provider: "agent-tools-fake", Model: "agent:Helper", UserMessage: "what is new?",
	})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.BeginStream(ctx, id)
	waitStreamFinished(t, svc, id)
	return id, drainAudit(ch)
}

// TestChatTurn_AgentHostedToolsAreAudited (R-provider-29): the accept-time chat.stream row exists before any
// hosted tool runs, so the turn's end records the hosted tool types the agent's run executed — once each, sorted,
// with the number of calls — under the stream's owner, the flow and its organisation, tied by stream id.
func TestChatTurn_AgentHostedToolsAreAudited(t *testing.T) {
	id, rows := runAgentToolsTurn(t, true)
	var got []*storageif.AuditEvent
	for _, ev := range rows {
		if ev.Action == audit.ActionChatAgentTools {
			got = append(got, ev)
		}
	}
	if len(got) != 1 {
		t.Fatalf("chat.agent_tools rows = %d (all rows %+v), want one at the turn's end", len(got), rows)
	}
	ev := got[0]
	if ev.Meta["tools"] != "mcp_call,web_search_call" || ev.Meta["calls"] != "3" {
		t.Errorf("meta = %v, want tools mcp_call,web_search_call and calls 3", ev.Meta)
	}
	if ev.Meta["streamId"] != id || ev.Meta["provider"] != "agent-tools-fake" || ev.Meta["model"] != "agent:Helper" {
		t.Errorf("meta = %v, want the stream %s, its provider and model", ev.Meta, id)
	}
	if ev.UserID != "u-owner" || ev.OrgID != "org-1" || ev.ResourceType != "flow" || ev.ResourceID != "f1" {
		t.Errorf("row = user %q org %q resource %s/%s, want u-owner, org-1, flow/f1", ev.UserID, ev.OrgID, ev.ResourceType, ev.ResourceID)
	}
}

// A turn in which no hosted tool ran records no chat.agent_tools row.
func TestChatTurn_NoHostedToolsNoAuditRow(t *testing.T) {
	_, rows := runAgentToolsTurn(t, false)
	for _, ev := range rows {
		if ev.Action == audit.ActionChatAgentTools {
			t.Fatalf("a turn without hosted tools recorded %+v", ev)
		}
	}
}
