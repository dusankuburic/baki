package service

import (
	"context"
	"encoding/json"

	"pad-core/logger"
	"pad-core/models"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// callerPreferences returns the settings that carry the CALLING user's chat preferences
// (custom instructions, history toggle): the user's own row in cloud, s.settings on
// desktop and when the row cannot be read. Deployment-wide limits (daily budgets)
// deliberately keep reading s.settings: a user must not be able to raise their own spend
// cap. Within a chat turn (withTurnMemo) the row is read once and shared by every reader.
func (s *ChatService) callerPreferences(ctx context.Context) *models.AppSettings {
	if memo := turnMemoFrom(ctx); memo != nil {
		memo.prefsOnce.Do(func() { memo.prefs = s.loadCallerPreferences(ctx) })
		return memo.prefs
	}
	return s.loadCallerPreferences(ctx)
}

// loadCallerPreferences is callerPreferences without the turn's memo.
func (s *ChatService) loadCallerPreferences(ctx context.Context) *models.AppSettings {
	if s.mode == config.ModeCloud && s.backend != nil {
		if claims := auth.ClaimsFromContext(ctx); claims != nil && claims.UserID != "" {
			stored, err := s.backend.LoadUserSettings(ctx, claims.UserID)
			if err == nil && stored != nil {
				if m, ok := storageSettingsToModel(stored); ok {
					return m
				}
			} else if err != nil {
				logger.Warn("chat: user settings unavailable; using deployment settings", "error", err)
			}
		}
	}
	if s.settings != nil {
		return s.settings.Get()
	}
	return nil
}

// conversationHistoryEnabled reports whether the caller's "Save conversation history"
// preference allows persisting chat turns; absent settings keep the historical default
// (saved).
func (s *ChatService) conversationHistoryEnabled(ctx context.Context) bool {
	prefs := s.callerPreferences(ctx)
	return prefs == nil || prefs.AI.SaveConversationHistory
}

// storageSettingsToModel bridges the storage settings type to the model type; the two
// are kept field-for-field identical (same JSON tags), so the JSON round trip is lossless.
func storageSettingsToModel(in *storageif.AppSettings) (*models.AppSettings, bool) {
	data, err := json.Marshal(in)
	if err != nil {
		return nil, false
	}
	var out models.AppSettings
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, false
	}
	return &out, true
}
