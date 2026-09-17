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

// userSettingsLoader is the part of the storage backend that holds each user's
// own preferences (cloud). The chat store interface does not include it, so it
// is detected rather than required.
type userSettingsLoader interface {
	LoadUserSettings(ctx context.Context, userID string) (*storageif.AppSettings, error)
}

// callerPreferences returns the settings that carry the CALLING user's chat
// preferences: custom instructions and whether conversation history is saved.
//
// Cloud saves preferences per user (Settings → UpdateUserSettings →
// user_settings), but chat only ever read s.settings — the server's own
// settings file — so in cloud mode a user's custom instructions and history
// toggle never reached their chat. Desktop has one user and one store, which
// is s.settings, and that is also the fallback when the user's row cannot be
// read. Deployment-wide limits (daily budgets) deliberately keep reading
// s.settings: a user must not be able to raise their own spend cap.
func (s *ChatService) callerPreferences(ctx context.Context) *models.AppSettings {
	if s.mode == config.ModeCloud && s.backend != nil {
		if loader, ok := s.backend.(userSettingsLoader); ok {
			if claims := auth.ClaimsFromContext(ctx); claims != nil && claims.UserID != "" {
				stored, err := loader.LoadUserSettings(ctx, claims.UserID)
				if err == nil && stored != nil {
					if m, ok := storageSettingsToModel(stored); ok {
						return m
					}
				} else if err != nil {
					logger.Warn("chat: user settings unavailable; using deployment settings", "error", err)
				}
			}
		}
	}
	if s.settings != nil {
		return s.settings.Get()
	}
	return nil
}

// conversationHistoryEnabled reports whether the caller's "Save conversation
// history" preference allows persisting chat turns. Absent settings keep the
// historical default (saved).
func (s *ChatService) conversationHistoryEnabled(ctx context.Context) bool {
	prefs := s.callerPreferences(ctx)
	return prefs == nil || prefs.AI.SaveConversationHistory
}

// storageSettingsToModel bridges the storage settings type to the model type.
// The two are kept field-for-field identical (same JSON tags), so the JSON
// round trip is lossless.
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
