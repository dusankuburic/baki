package service

import (
	"context"
	"errors"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/rag"
	"pad-core/ai/scrubber"
	"pad-core/models"
)

// The search_knowledge tool (R-knowledge-6). The turn's guidelines are fetched once, before the turn, from the
// question; in the tool loop the model learns from list_findings and get_block which rule, connector or pattern
// matters, and asks the organisation's knowledge base itself. The tool is the same lookup as the turn's — the
// membership gate, the query scrub, the guidelines cap — counted under its own source, and it never emits the
// turn's knowledge notice: its result tells the model.

// errKnowledgeNotAvailable answers a caller who is not a member of the flow's organisation: the knowledge base is
// the organisation's, and this path runs without row-level security, so membership is the gate (fail closed).
var errKnowledgeNotAvailable = errors.New("the organisation's knowledge base is not available to you in this conversation")

// knowledgeWired reports whether a turn on doc has a knowledge base: the flow belongs to an organisation and the
// knowledge service is wired (never on the desktop or in local mode without one).
func (s *ChatService) knowledgeWired(doc *models.FlowDocument) bool {
	return s.knowledge != nil && doc != nil && doc.OrganizationID != ""
}

// toolGate is the session's tool gate (ADR 0038): what the stream's actor may run, whether approvals and the fix
// tools are available, and whether the flow has a knowledge base for search_knowledge.
func (s *ChatService) toolGate(ctx context.Context, doc *models.FlowDocument) ai.ToolGate {
	return ai.ToolSession{
		Actor:          auth.ActorFromContext(ctx),
		ApprovalsWired: s.flowCache != nil,
		CanEditFlow:    s.canEditFlow(ctx, doc),
		Knowledge:      s.knowledgeWired(doc),
	}.Gate()
}

// knowledgeTool is the session's knowledge base for search_knowledge, the caller's (scope) in the flow's
// organisation; nil when the turn has none.
func (s *ChatService) knowledgeTool(scope string, doc *models.FlowDocument) ai.ToolKnowledge {
	if !s.knowledgeWired(doc) {
		return nil
	}
	return chatKnowledgeTool{s: s, scope: scope, orgID: doc.OrganizationID}
}

type chatKnowledgeTool struct {
	s     *ChatService
	scope string
	orgID string
}

// SearchKnowledge answers the model's search: the guidelines page for query, scrubbed and cut to the turn's
// guidelines cap, or an error it can read. The search's follow-up (an empty result's census) runs after, off the
// tool's path.
func (k chatKnowledgeTool) SearchKnowledge(ctx context.Context, query string, limit int) (string, error) {
	if !k.s.isOrgMember(ctx, k.orgID, k.scope) {
		recordRAGLookup(ragSourceTool, ragOutcomeNonMember)
		return "", errKnowledgeNotAvailable
	}
	l, outcome, err := k.s.searchOrgKnowledge(ctx, ragSourceTool, k.scope, k.orgID, query, limit)
	if l.followUp != nil {
		go runRAGFollowUp(ctx, l.followUp)
	}
	switch outcome {
	case ragOutcomeHit:
		return scrubber.ScrubText(l.guidelines), nil
	case ragOutcomeMiss:
		return "No passage of the organisation's knowledge base matches the query.", nil
	case ragOutcomeDeadline:
		return "", errors.New("the knowledge base did not answer in time — try again with a shorter, more specific query")
	case ragOutcomeIdentityMismatch:
		return "", rag.ErrEmbedderMismatch
	case ragOutcomeNoEmbedder:
		return "", errors.New("you have no embedding provider the organisation's knowledge base can be searched with (no key of an embedding-capable provider)")
	}
	if err != nil {
		// The cause is logged (searchOrgKnowledge); the model gets the fact, not the store's or provider's text.
		return "", errors.New("the knowledge base search failed")
	}
	return "", errors.New("the knowledge base search did not finish")
}
