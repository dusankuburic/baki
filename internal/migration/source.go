package migration

import (
	"context"

	"pad-analyzer/internal/storage/interfaces"
)

// LegacySource is the read-only view of a legacy data directory that the migrator copies from: exactly what Migrate
// reads, and nothing it could write through. filesystem.OpenLegacySource implements it over a PAD_DATA_DIR; the
// contract is contract.RunLegacySourceSuite.
type LegacySource interface {
	// ListFlowIDs returns up to limit flow IDs greater than afterID, in ascending order. The walk pages by the
	// immutable ID, so a page boundary can neither skip nor repeat a flow. A stored flow is listed even when it
	// cannot be decoded: the copy then reports it as failed instead of leaving it behind unnoticed.
	ListFlowIDs(ctx context.Context, afterID string, limit int) ([]string, error)
	// LoadFlow returns the flow stored under id with its content, or interfaces.ErrNotFound.
	LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error)
	// LoadSettings returns the deployment settings, or interfaces.ErrNotFound when the source holds none.
	LoadSettings(ctx context.Context) (*interfaces.AppSettings, error)
	// ListConversations names every stored conversation, in a stable order.
	ListConversations(ctx context.Context) ([]ConversationRef, error)
	// LoadConversation reads one conversation named by ListConversations.
	LoadConversation(ctx context.Context, ref ConversationRef) (*LegacyConversation, error)
}

// ConversationRef names one stored conversation of a LegacySource.
type ConversationRef struct {
	Scope  string // the provider or context key the thread was stored under
	FlowID string
	// Path is where the source keeps the conversation, for logs and Result.Errors.
	Path string
}

// LegacyConversation is one stored chat thread in the destination's message shape.
type LegacyConversation struct {
	FlowID   string
	Scope    string
	Messages []interfaces.ChatMessage
}
