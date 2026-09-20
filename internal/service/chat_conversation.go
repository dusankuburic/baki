package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/config"
	"pad-core/logger"
	"pad-core/models"
)

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
	if doc == nil {
		return fmt.Errorf("no flow loaded")
	}
	// "Save conversation history" off: accept and drop. The toggle used to have
	// no reader, so turns were stored regardless of it. The client stops
	// calling this and sends history with each turn instead; the check here
	// also covers the server's own start-of-stream persist and older clients.
	if !s.conversationHistoryEnabled(ctx) {
		return nil
	}
	trimmed := evictConvMessages(messages)

	if s.useBackendConversations() {
		return s.backend.SaveConversation(ctx, doc.ID, provider, toStorageMessages(trimmed))
	}

	path, err := convFilePath(s.configDir, provider, doc.ID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("failed to create conversations directory: %w", err)
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
		return fmt.Errorf("failed to marshal conversation: %w", err)
	}

	// Atomic write at 0600: an interrupted write leaves the prior conversation
	// intact rather than truncating it, and the file isn't world-readable.
	return atomicWriteConv(dir, path, data)
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
// list with the same content.
func (s *ChatService) persistAssistantTurn(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest, ctl *streamCtl) {
	if doc == nil || ctl == nil {
		return
	}
	ctl.mu.Lock()
	answer := ctl.buffer.String()
	done, errMsg := ctl.done, ctl.errMsg
	ctl.mu.Unlock()
	if !done || errMsg != "" || strings.TrimSpace(answer) == "" {
		return
	}

	convKey := req.ConversationKey()
	// The stream's context is cancelled as the worker unwinds; this write must
	// still land.
	writeCtx := context.WithoutCancel(ctx)
	history, err := s.GetConversation(writeCtx, doc, convKey)
	if err != nil {
		logger.Warn("chat: could not load conversation to store the answer", "error", err)
		return
	}
	if n := len(history); n > 0 && history[n-1].Role == "assistant" && history[n-1].Content == answer {
		return // already stored (a retry, or the client got there first)
	}
	history = append(history, models.ChatMessage{
		ID:        uuid.NewString(),
		Role:      "assistant",
		Content:   answer,
		Timestamp: time.Now(),
		Provider:  req.Provider,
		Model:     req.Model,
	})
	if err := s.SaveConversation(writeCtx, doc, convKey, history); err != nil {
		logger.Warn("chat: failed to store the finished answer", "error", err)
	}
}
