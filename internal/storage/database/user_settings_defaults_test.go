package database_test

import (
	"context"
	"testing"
	"time"
)

// TestPostgres_LoadUserSettings_NoRowReturnsApplicationDefaults: a user who has
// never saved settings used to get a bare {Version: 1}. The client merged its
// zero values over its own defaults, so every default of true showed as off
// for a new cloud account, and chat, which honours "Save conversation history",
// would have stopped saving their conversations.
func TestPostgres_LoadUserSettings_NoRowReturnsApplicationDefaults(t *testing.T) {
	b := openTestDB(t)
	got, err := b.LoadUserSettings(context.Background(), "never-saved-"+itoa(time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("LoadUserSettings: %v", err)
	}
	if !got.AI.SaveConversationHistory {
		t.Error("saveConversationHistory must default to true")
	}
	if !got.Analysis.AutoAnalyzeOnOpen {
		t.Error("autoAnalyzeOnOpen must default to true")
	}
	if got.Parser.MaxFileSizeMB <= 0 {
		t.Errorf("maxFileSizeMB must carry its default, got %d", got.Parser.MaxFileSizeMB)
	}
}
