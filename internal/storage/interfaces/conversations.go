package interfaces

import "context"

// ConversationStore covers persisted chat conversation history.
type ConversationStore interface {
	SaveConversation(ctx context.Context, flowID, scope string, messages []ChatMessage) error
	LoadConversation(ctx context.Context, flowID, scope string) ([]ChatMessage, error)
	DeleteConversation(ctx context.Context, flowID, scope string) error
}

// ToolCallRecord mirrors models.ToolCallRecord (same JSON tags) — the per-message tool transparency trail.
type ToolCallRecord struct {
	Name       string `json:"name"`
	Label      string `json:"label,omitempty"`
	Ok         bool   `json:"ok"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// FixItemSnapshot mirrors models.FixItemSnapshot — one fix inside a batch approval record.
type FixItemSnapshot struct {
	RuleID     string `json:"ruleId"`
	FixType    string `json:"fixType"`
	BlockLabel string `json:"blockLabel"`
	Line       int    `json:"line"`
	Summary    string `json:"summary"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

// FixProposalSnapshot mirrors models.FixProposalSnapshot — the persisted approval record (single or batch).
type FixProposalSnapshot struct {
	ProposalID  string            `json:"proposalId"`
	Tool        string            `json:"tool,omitempty"`
	PreviewKind string            `json:"previewKind,omitempty"`
	RuleID      string            `json:"ruleId"`
	FixType     string            `json:"fixType"`
	BlockLabel  string            `json:"blockLabel"`
	Line        int               `json:"line"`
	Summary     string            `json:"summary"`
	Status      string            `json:"status"`
	Message     string            `json:"message,omitempty"`
	Items       []FixItemSnapshot `json:"items,omitempty"`
}

// ChatMessage mirrors models.ChatMessage for persistence; ServedModel and billed rates ride inside the messages JSON — no columns of their own.
type ChatMessage struct {
	ID               string                `json:"id"`
	Role             string                `json:"role"`
	Content          string                `json:"content"`
	Timestamp        string                `json:"timestamp"`
	ContextBlockID   string                `json:"contextBlockId,omitempty"`
	ContextSubflowID string                `json:"contextSubflowId,omitempty"`
	TokensIn         int                   `json:"tokensIn,omitempty"`
	TokensOut        int                   `json:"tokensOut,omitempty"`
	Provider         string                `json:"provider,omitempty"`
	Model            string                `json:"model,omitempty"`
	ModelLabel       string                `json:"modelLabel,omitempty"`
	ServedModel      string                `json:"servedModel,omitempty"`
	InputCostPerM    float64               `json:"inputCostPerM,omitempty"`
	OutputCostPerM   float64               `json:"outputCostPerM,omitempty"`
	FinishReason     string                `json:"finishReason,omitempty"`
	ToolCalls        []ToolCallRecord      `json:"toolCalls,omitempty"`
	FixProposal      *FixProposalSnapshot  `json:"fixProposal,omitempty"` // legacy singular
	FixProposals     []FixProposalSnapshot `json:"fixProposals,omitempty"`
}
