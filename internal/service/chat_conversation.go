package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pad-analyzer/internal/config"
	"pad-core/logger"
	"pad-core/models"
)

// reconstructHistory loads the stored conversation for the request's context
// key so a client that omitted Messages (the efficient path — it sends only the
// new userMessage) still gets full LLM context. The key matches the client's
// save key: contextBlockId, or "flow" when none. A store error degrades to
// empty history rather than failing the turn. Returns nil when doc is nil.
func (s *ChatService) reconstructHistory(ctx context.Context, doc *models.FlowDocument, req models.ChatRequest) []models.ChatMessage {
	if doc == nil {
		return nil
	}
	convKey := req.ContextBlockID
	if convKey == "" {
		convKey = "flow"
	}
	loaded, err := s.GetConversation(ctx, doc, convKey)
	if err != nil {
		logger.Warn("chat history reconstruction failed; continuing with empty history", "error", err)
		return nil
	}
	return loaded
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

func (s *ChatService) ExportConversation(ctx context.Context, doc *models.FlowDocument, provider string, path string) error {
	if err := validateUserPath(path); err != nil {
		return err
	}
	msgs, err := s.GetConversation(ctx, doc, provider)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "### %s (%s)\n\n%s\n\n", m.Role, m.Timestamp.Format(time.RFC3339), m.Content)
	}
	return os.WriteFile(path, []byte(b.String()), 0644) // #nosec G306 -- user-chosen export path; world-readable is intended for sharing
}
