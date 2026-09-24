package service

import (
	"encoding/json"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// The storage and model settings structs are bridged by a JSON round-trip, so
// a field that exists on one side only is silently dropped. These pin the
// fields the frontend relies on.

func TestSettingsBridge_KeepsChatTextSize(t *testing.T) {
	m := models.DefaultSettings()
	m.Appearance.ChatTextSize = "lg"

	stored := settingsFromModel(m)
	if stored == nil {
		t.Fatal("settingsFromModel returned nil")
	}
	if stored.Appearance.ChatTextSize != "lg" {
		t.Fatalf("storage ChatTextSize = %q, want %q", stored.Appearance.ChatTextSize, "lg")
	}

	back := settingsToModel(stored)
	if back.Appearance.ChatTextSize != "lg" {
		t.Fatalf("model ChatTextSize = %q, want %q", back.Appearance.ChatTextSize, "lg")
	}
}

func TestSettingsBridge_DefaultChatTextSize(t *testing.T) {
	if got := models.DefaultSettings().Appearance.ChatTextSize; got != "md" {
		t.Fatalf("default ChatTextSize = %q, want %q", got, "md")
	}
}

// Stored settings written by older builds still carry layout.chatPanelHeight
// (the docked chat's height handle, since removed). They must keep decoding.
func TestSettingsBridge_ToleratesLegacyChatPanelHeight(t *testing.T) {
	raw := []byte(`{"version":1,"layout":{"sidebarWidth":280,"inspectorWidth":320,"chatPanelHeight":420},"appearance":{"theme":"dark"}}`)

	var stored storageif.AppSettings
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal legacy settings: %v", err)
	}
	if stored.Layout.InspectorWidth != 320 {
		t.Fatalf("InspectorWidth = %d, want 320", stored.Layout.InspectorWidth)
	}
	m := settingsToModel(&stored)
	if m.Layout.InspectorWidth != 320 {
		t.Fatalf("model InspectorWidth = %d, want 320", m.Layout.InspectorWidth)
	}
}
