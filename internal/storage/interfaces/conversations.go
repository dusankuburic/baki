package interfaces

import (
	"context"
	"fmt"
)

// ErrConversationConflict is returned by SaveConversationIf and DeleteConversationIf when the stored conversation is
// no longer at the version the caller read. It wraps ErrVersionConflict, so errors.Is matches both and the generic
// mapping to HTTP 409 applies; the API answers it with the CONVERSATION_CONFLICT code.
var ErrConversationConflict = fmt.Errorf("conversation changed: %w", ErrVersionConflict)

// ConversationStore covers persisted chat conversation history.
//
// A conversation has a version (ADR 0031): 0 for a conversation that does not exist or that was last written before
// versions did, otherwise a positive number the store assigns and never reuses. Every change to the stored messages
// gives the conversation a new version, whichever method made it, so the *If methods detect a blind
// SaveConversation too. SaveConversation and DeleteConversation stay for callers that have no version to hold (the
// legacy migrator, a client that sends no base version).
type ConversationStore interface {
	SaveConversation(ctx context.Context, flowID, scope string, messages []ChatMessage) error
	LoadConversation(ctx context.Context, flowID, scope string) ([]ChatMessage, error)
	DeleteConversation(ctx context.Context, flowID, scope string) error

	// LoadConversationVersioned is LoadConversation plus the version the messages were read at. A missing
	// conversation is a non-nil empty list at version 0.
	LoadConversationVersioned(ctx context.Context, flowID, scope string) ([]ChatMessage, int64, error)
	// SaveConversationIf replaces the messages only while the conversation is still at expected (0 creates it, or
	// replaces a conversation that has never had a version) and returns the new version; storing the list already
	// stored keeps the version. Otherwise it writes nothing and returns ErrConversationConflict together with the
	// conversation's current version (0 when it is gone), so the caller can say what it conflicted with.
	SaveConversationIf(ctx context.Context, flowID, scope string, expected int64, messages []ChatMessage) (int64, error)
	// DeleteConversationIf removes the conversation only while it is at expected, else ErrConversationConflict.
	// A missing conversation is a no-op for expected 0 and a conflict for any other version: it changed since.
	DeleteConversationIf(ctx context.Context, flowID, scope string, expected int64) error
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
	FinishReason     string                `json:"finishReason,omitempty"`
	ToolCalls        []ToolCallRecord      `json:"toolCalls,omitempty"`
	FixProposal      *FixProposalSnapshot  `json:"fixProposal,omitempty"` // legacy singular
	FixProposals     []FixProposalSnapshot `json:"fixProposals,omitempty"`
}
