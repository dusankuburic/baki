package interfaces

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrConversationConflict is returned by SaveConversationIf and DeleteConversationIf when the stored conversation is
// no longer at the version the caller read. It wraps ErrVersionConflict, so errors.Is matches both and the generic
// mapping to HTTP 409 applies; the API answers it with the CONVERSATION_CONFLICT code.
var ErrConversationConflict = fmt.Errorf("conversation changed: %w", ErrVersionConflict)

// ErrConversationKeyTaken is returned by a write (SaveConversationIf, SetConversationTitle) whose key names another
// user's conversation on the flow (R-security-3): the key holds one conversation until the table is keyed by owner,
// so the caller's cannot be stored under it. Not a conflict: no reload makes the write succeed, a new key does. The
// API answers it 409 CONVERSATION_KEY_TAKEN.
var ErrConversationKeyTaken = errors.New("this conversation key belongs to another user's conversation")

// ErrConversationUnowned is returned by DeleteConversationIf for a conversation stored before conversations had
// owners (migration 0052) when the caller may not delete those: nobody can tell whose it is, so only someone who may
// delete the whole flow may remove it. The API answers it 403 CONVERSATION_UNOWNED.
var ErrConversationUnowned = errors.New("this conversation was stored before conversations had owners; only someone who may delete the flow can delete it")

// ConversationStore covers persisted chat conversation history.
//
// A conversation has a version (ADR 0031): 0 for a conversation that does not exist or that was last written before
// versions did, otherwise a positive number the store assigns and never reuses. Every change to the stored messages
// gives the conversation a new version, whichever method made it, so the *If methods detect a blind
// SaveConversation too.
//
// A conversation has an owner (R-security-3, migration 0052): the user whose write created it, never changed once
// set. The methods that take an owner act for that user: they read, change and delete only the owner's conversations
// and the unowned ones (stored before owners existed, or by the release before), and create the owner's. A key under
// which another user's conversation is stored reads as missing and refuses writes (ErrConversationKeyTaken); a delete
// of it is the delete of a missing conversation. The empty owner is the deployment without accounts and the system:
// it sees every conversation and creates unowned ones. SaveConversation, LoadConversation and DeleteConversation take
// no owner: they are the system's (the store's own tests, a system write), never a request's.
//
// An unowned conversation under a thread id (IsThreadID) that none of its messages names as its context block is a
// client's thread from before owners: its id is unguessable, so whoever writes it first with an owner — a save, a
// title — is its creator and claims it, and whoever presents the id may delete it. Under any other key ("flow", a
// block id, which every viewer of the flow sees) an unowned conversation is shared (IsSharedConversation): never
// claimed, appended to by the flow's chat audience, deleted or named only with the right to delete the flow.
type ConversationStore interface {
	SaveConversation(ctx context.Context, flowID, scope string, messages []ChatMessage) error
	LoadConversation(ctx context.Context, flowID, scope string) ([]ChatMessage, error)
	DeleteConversation(ctx context.Context, flowID, scope string) error

	// LoadConversationVersioned is LoadConversation plus the version the messages were read at, as owner sees it. A
	// missing conversation, or another user's, is a non-nil empty list at version 0.
	LoadConversationVersioned(ctx context.Context, flowID, scope, owner string) ([]ChatMessage, int64, error)
	// SaveConversationIf replaces the messages only while the conversation is still at expected (0 creates it, or
	// replaces a conversation that has never had a version) and returns the new version; storing the list already
	// stored keeps the version. Otherwise it writes nothing and returns ErrConversationConflict together with the
	// conversation's current version (0 when it is gone), so the caller can say what it conflicted with. A created
	// conversation is owner's; an unowned one becomes owner's when it is claimable (above), else stays unowned.
	// Another user's: ErrConversationKeyTaken, nothing written.
	SaveConversationIf(ctx context.Context, flowID, scope, owner string, expected int64, messages []ChatMessage) (int64, error)
	// DeleteConversationIf removes the conversation only while it is at expected, else ErrConversationConflict.
	// A missing conversation (another user's included) is a no-op for expected 0 and a conflict for any other
	// version: it changed since. A shared conversation (IsSharedConversation) is removed only with mayDeleteUnowned
	// (the caller may delete the flow), else ErrConversationUnowned; a claimable unowned one by any owner.
	DeleteConversationIf(ctx context.Context, flowID, scope, owner string, expected int64, mayDeleteUnowned bool) error
	// IsSharedConversation reports whether an unowned conversation that no write claims is stored under the key: a
	// client's replacement of its list needs the right to delete the flow.
	IsSharedConversation(ctx context.Context, flowID, scope string) (bool, error)
	// ListConversations is owner's conversations on the flow, newest change first (updated_at, then the key, both
	// descending), after the position after (nil: from the start), at most limit. Unowned conversations are in
	// nobody's list. The empty owner has no list (nil).
	ListConversations(ctx context.Context, flowID, owner string, after *PageCursor, limit int) ([]ConversationSummary, error)
	// SetConversationTitle names owner's conversation; "" removes the name. The version and the messages stay. A
	// claimable unowned conversation is claimed. A missing conversation under a thread id is created empty with the
	// name, so a thread named before its first turn keeps it; under any other key it is ErrNotFound (a title must not
	// take a predictable key from the other users). It returns the conversation's version after the call (the created
	// one's, for a missing conversation). Another user's conversation: ErrConversationKeyTaken; a shared one:
	// ErrConversationUnowned (it has no owner to name it for). The empty owner names nothing (ErrConversationUnowned).
	SetConversationTitle(ctx context.Context, flowID, scope, owner, title string) (int64, error)
}

// IsThreadID reports whether a conversation key is a client-generated thread id: a uuid in its canonical lower-case
// form, as the client generates them (crypto.randomUUID) and the stream accepts as conversationId.
func IsThreadID(key string) bool {
	u, err := uuid.Parse(key)
	return err == nil && u.String() == key
}

// NamesContextBlock reports whether any message names key as its context block: the conversation was stored under a
// block id (the legacy key of a block-scoped chat), not under a thread id, though both are uuids.
func NamesContextBlock(key string, messages []ChatMessage) bool {
	for _, m := range messages {
		if m.ContextBlockID == key {
			return true
		}
	}
	return false
}

// ConversationTitleMaxRunes bounds a stored conversation title; the service clamps to it.
const ConversationTitleMaxRunes = 200

// ConversationSummary is one entry of a user's thread index on a flow (ListConversations): the conversation's key,
// its owner-given title ("" when unnamed), when it was created and last changed, how many messages it holds and its
// version (what a save or clear holds).
type ConversationSummary struct {
	Scope        string
	Title        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	MessageCount int
	Version      int64
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
	ID         string `json:"id,omitempty"`
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
	ErrorCode        string                `json:"errorCode,omitempty"`
	ErrorShared      bool                  `json:"errorShared,omitempty"`
	ToolCalls        []ToolCallRecord      `json:"toolCalls,omitempty"`
	FixProposal      *FixProposalSnapshot  `json:"fixProposal,omitempty"` // legacy singular
	FixProposals     []FixProposalSnapshot `json:"fixProposals,omitempty"`
	Truncated        int                   `json:"truncated,omitempty"`
	Knowledge        string                `json:"knowledge,omitempty"`
}
