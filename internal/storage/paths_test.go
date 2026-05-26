package storage

import (
	"strings"
	"testing"
)

func TestConfigDir_ReturnsNonEmpty(t *testing.T) {
	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() error: %v", err)
	}
	if dir == "" {
		t.Error("ConfigDir() returned empty string")
	}
	if !strings.Contains(dir, "pad-analyzer") {
		t.Errorf("ConfigDir() = %q, expected to contain 'pad-analyzer'", dir)
	}
}

func TestCacheDir_ReturnsNonEmpty(t *testing.T) {
	dir, err := CacheDir()
	if err != nil {
		t.Fatalf("CacheDir() error: %v", err)
	}
	if dir == "" {
		t.Error("CacheDir() returned empty string")
	}
	if !strings.Contains(dir, "pad-analyzer") {
		t.Errorf("CacheDir() = %q, expected to contain 'pad-analyzer'", dir)
	}
}

func TestLogDir_IsSubdirOfConfigDir(t *testing.T) {
	configDir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() error: %v", err)
	}
	logDir, err := LogDir()
	if err != nil {
		t.Fatalf("LogDir() error: %v", err)
	}
	if !strings.HasPrefix(logDir, configDir) {
		t.Errorf("LogDir %q should be under ConfigDir %q", logDir, configDir)
	}
	if !strings.Contains(logDir, "logs") {
		t.Errorf("LogDir %q should contain 'logs'", logDir)
	}
}

func TestSettingsPath_EndsWithSettingsJSON(t *testing.T) {
	path, err := SettingsPath()
	if err != nil {
		t.Fatalf("SettingsPath() error: %v", err)
	}
	if path == "" {
		t.Error("SettingsPath() returned empty string")
	}
	if !strings.HasSuffix(path, "settings.json") {
		t.Errorf("SettingsPath() = %q, expected to end with 'settings.json'", path)
	}
}

func TestNewSettingsStore_UsesRealPath(t *testing.T) {
	// NewSettingsStore uses SettingsPath() (real user config dir).
	// It should not error on a standard developer machine.
	store, err := NewSettingsStore()
	if err != nil {
		t.Fatalf("NewSettingsStore() error: %v", err)
	}
	settings := store.Get()
	if settings == nil {
		t.Error("Get() returned nil settings after NewSettingsStore")
	}
}
