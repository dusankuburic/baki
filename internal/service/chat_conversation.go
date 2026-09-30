package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"

	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// ClampModelLabel is a client-sent ChatMessage.ModelLabel as a conversation
// save (/api/chat/save) stores it: NFC-normalised; control, bidi and
// zero-width characters (Unicode Cc and Cf) dropped; whitespace runs — tabs
// and newlines included — collapsed to one space and trimmed; at most 64
// characters, a registry display name's limit. The label is only the name
// shown next to an answer, so it is clamped rather than refused (a refusal
// would fail the whole save); what the clamp removes is what lets a label
// impersonate another model's name.
func ClampModelLabel(label string) string {
	if label == "" {
		return ""
	}
	label = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.In(r, unicode.Cc, unicode.Cf):
			return -1
		}
		return r
	}, strings.ToValidUTF8(label, ""))
	label = strings.Join(strings.Fields(norm.NFC.String(label)), " ")
	return strings.TrimSpace(truncateRunes(label, storageif.AzureNameMaxLen))
}

// reconstructHistory loads the stored conversation for the request's
// conversation key so a client that omitted Messages (the efficient path — it
// sends only the new userMessage) still gets full LLM context. The key matches
// the client's save key (see ChatRequest.ConversationKey). Returns nil when
// doc is nil.
func (s *ChatService) reconstructHistory(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest) ([]models.ChatMessage, error) {
	if doc == nil {
		return nil, nil
	}
	loaded, err := s.GetConversation(ctx, doc, req.ConversationKey())
	if err != nil {
		// The error is RETURNED, not swallowed. A caller that persists after
		// reconstructing must be able to tell "this conversation is empty" from
		// "I could not read this conversation" — SaveConversation is a full
		// replace, so writing on the second is data loss. See
		// reconstructAndPersistUserTurn.
		return nil, err
	}
	return loaded, nil
}

// clientMessageID returns the client-supplied id for a persisted turn when it
// is a UUID (see ChatRequest.UserMessageID/AssistantMessageID), otherwise a
// fresh server-minted one. Sharing the client's id is what lets a client that
// reloads the conversation mid-turn dedupe the server's copy by id.
func clientMessageID(id string) string {
	// Canonical 8-4-4-4-12 form only: uuid.Parse also accepts braced, urn: and
	// 32-hex spellings, which the client's own id comparison would not match.
	if len(id) == 36 {
		if u, err := uuid.Parse(id); err == nil && strings.EqualFold(u.String(), id) {
			return id
		}
	}
	return uuid.NewString()
}

// useBackendConversations reports whether conversations should persist through
// the storage backend (cloud mode: Postgres, durable + cross-replica + RLS)
// rather than the local on-disk file store (desktop, single persistent instance).
// Explicit allow-list on ModeCloud: an unset/zero mode (or any future mode) must
// fall back to the local file store, never silently route chat history to Postgres.
func (s *ChatService) useBackendConversations() bool {
	return s.mode == config.ModeCloud && s.backend != nil
}

func (s *ChatService) GetConversation(ctx context.Context, doc *models.FlowDocument, provider string) ([]models.ChatMessage, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	if s.useBackendConversations() {
		stored, err := s.backend.LoadConversation(ctx, doc.ID, provider)
		if err != nil {
			return nil, err
		}
		return toModelMessages(stored), nil
	}
	path, err := convFilePath(s.configDir, provider, doc.ID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path derived from configDir+provider+flowID, not raw user input
	if err != nil {
		if os.IsNotExist(err) {
			return []models.ChatMessage{}, nil
		}
		return nil, fmt.Errorf("failed to read conversation file: %w", err)
	}

	var conv models.ConversationFile
	if err := json.Unmarshal(data, &conv); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation: %w", err)
	}
	return conv.Messages, nil
}

func (s *ChatService) SaveConversation(ctx context.Context, doc *models.FlowDocument, provider string, messages []models.ChatMessage) error {
	_, err := s.saveConversation(ctx, doc, provider, messages)
	return err
}

// saveConversation is SaveConversation reporting whether the list was stored:
// false with a nil error when the caller turned "Save conversation history"
// off (the save is accepted and dropped).
func (s *ChatService) saveConversation(ctx context.Context, doc *models.FlowDocument, provider string, messages []models.ChatMessage) (stored bool, err error) {
	if doc == nil {
		return false, fmt.Errorf("no flow loaded")
	}
	// "Save conversation history" off: accept and drop. The toggle used to have
	// no reader, so turns were stored regardless of it. The client stops
	// calling this and sends history with each turn instead; the check here
	// also covers the server's own start-of-stream persist and older clients.
	if !s.conversationHistoryEnabled(ctx) {
		return false, nil
	}
	// An assistant message with blank content and no tool calls or fix
	// proposals is dropped before storing (F-10), mirroring
	// persistAssistantTurn's own blank rule: an empty 'stop' answer saved as
	// an empty assistant message used to brick the thread on Claude, whose
	// Messages API rejects {"role":"assistant","content":""} on every later
	// turn. Tool-bearing and fix-bearing turns stay (their content is not the
	// answer).
	filtered := make([]models.ChatMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" &&
			len(m.ToolCalls) == 0 && len(m.FixProposals) == 0 && m.FixProposal == nil {
			continue
		}
		filtered = append(filtered, m)
	}
	trimmed := evictConvMessages(filtered)

	if s.useBackendConversations() {
		if err := s.backend.SaveConversation(ctx, doc.ID, provider, toStorageMessages(trimmed)); err != nil {
			return false, err
		}
		return true, nil
	}

	path, err := convFilePath(s.configDir, provider, doc.ID)
	if err != nil {
		return false, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return false, fmt.Errorf("failed to create conversations directory: %w", err)
	}

	conv := models.ConversationFile{
		Version:   1,
		FlowKey:   doc.ID,
		Scope:     provider,
		UpdatedAt: time.Now(),
		Messages:  trimmed,
	}

	data, err := json.MarshalIndent(conv, "", "  ")
	if err != nil {
		return false, fmt.Errorf("failed to marshal conversation: %w", err)
	}

	// Atomic write at 0600: an interrupted write leaves the prior conversation
	// intact rather than truncating it, and the file isn't world-readable.
	if err := atomicWriteConv(dir, path, data); err != nil {
		return false, err
	}
	return true, nil
}

func (s *ChatService) ClearConversation(ctx context.Context, doc *models.FlowDocument, provider string) error {
	if doc == nil {
		return fmt.Errorf("no flow loaded")
	}
	if s.useBackendConversations() {
		return s.backend.DeleteConversation(ctx, doc.ID, provider)
	}
	path, err := convFilePath(s.configDir, provider, doc.ID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete conversation file: %w", err)
	}
	return nil
}

// persistAnswerTimeout bounds the detached end-of-stream write of the answer
// (persistAssistantTurn). The write runs under the conversation's stripe mutex
// after the stream's own context is gone: unbounded, a stalled store parked
// every finishing stream there for good, holding the stripe every new stream
// on that conversation needs (mu.Lock is not ctx-aware) and, before the
// cleanup reordering in StreamChatMessage, its activeStreams slot. A variable
// so tests can shorten it.
var persistAnswerTimeout = 15 * time.Second

// persistAssistantTurn appends the finished answer to the stored conversation.
//
// The question is persisted when the stream starts (persistInitialUserTurn) but
// the answer was left to the client's save-on-done call, so a browser tab closed
// mid-answer — or any caller that simply does not call /api/chat/save — left the
// conversation holding a question with nothing beside it. Only runs on the
// server-reconstructed path: a client that sent its own Messages owns
// persistence, and SaveConversation still honours the history preference.
//
// Best-effort and idempotent in effect: the client's own save replaces the whole
// list with the same content. Bounded by persistAnswerTimeout: a store that
// hangs turns into a logged warning, never a held stripe.
//
// model and modelLabel are what the answer is stored with (turnModel's
// requested id and label): the id the client asked for — an Azure AI Foundry
// registry entry's "m/<entryId>", never the deployment it resolved to — and
// the entry's display name at send time, so history keeps naming the model
// after it is renamed or removed.
func (s *ChatService) persistAssistantTurn(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest, ctl *streamCtl, model, modelLabel string) {
	if doc == nil || ctl == nil {
		return
	}
	ctl.mu.Lock()
	answer := ctl.buffer.String()
	done, errMsg := ctl.done, ctl.errMsg
	tokensIn, tokensOut := ctl.tokensIn, ctl.tokensOut
	served, inRate, outRate := ctl.servedModel, ctl.inputCostPerM, ctl.outputCostPerM
	ctl.mu.Unlock()
	if !done || errMsg != "" || strings.TrimSpace(answer) == "" {
		return
	}

	convKey := req.ConversationKey()
	// The stream's context is cancelled as the worker unwinds; this write must
	// still land — within a bound (see persistAnswerTimeout).
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistAnswerTimeout)
	defer cancel()
	// Same critical section as reconstruct + persist-user-turn: the next turn
	// on this conversation may already be reading the history this replaces,
	// and SaveConversation is a full-file replace.
	mu := s.convMutexFor(doc.ID, convKey)
	mu.Lock()
	defer mu.Unlock()
	history, err := s.GetConversation(writeCtx, doc, convKey)
	if err != nil {
		logger.Warn("chat: could not load conversation to store the answer", "error", err)
		return
	}
	answerID := clientMessageID(req.AssistantMessageID)
	// Already stored: a retry, or the client got there first. Matching by id
	// anywhere in the history also covers the client's own interrupted partial
	// for this turn (the user pressed Stop just as the stream finished) even
	// once its next question has landed — appending the full answer then would
	// show the reply twice, out of order.
	if slices.ContainsFunc(history, func(m models.ChatMessage) bool { return m.ID == answerID }) {
		return
	}
	if n := len(history); n > 0 && history[n-1].Role == "assistant" && history[n-1].Content == answer {
		return
	}
	// Usage, served model and billed rates as the client saves them on
	// done, so a reload after a disconnected turn keeps the tag and prices
	// the turn at what it cost.
	history = append(history, models.ChatMessage{
		ID:             answerID,
		Role:           "assistant",
		Content:        answer,
		Timestamp:      time.Now(),
		Provider:       req.Provider,
		Model:          model,
		ModelLabel:     modelLabel,
		TokensIn:       tokensIn,
		TokensOut:      tokensOut,
		ServedModel:    served,
		InputCostPerM:  inRate,
		OutputCostPerM: outRate,
	})
	if err := s.SaveConversation(writeCtx, doc, convKey, history); err != nil {
		logger.Warn("chat: failed to store the finished answer", "error", err)
	}
}
