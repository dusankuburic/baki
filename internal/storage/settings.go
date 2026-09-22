package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"pad-core/logger"
	"pad-core/models"
)

const CurrentSettingsVersion = 1

type SettingsStore struct {
	mu      sync.RWMutex
	path    string
	current *models.AppSettings
}

func NewSettingsStore() (*SettingsStore, error) {
	path, err := SettingsPath()
	if err != nil {
		return nil, fmt.Errorf("getting settings path: %w", err)
	}
	return NewSettingsStoreAt(path)
}

// NewInMemorySettingsStore creates a SettingsStore that never touches the
// filesystem. Cloud mode uses it: deployment settings there live in Postgres
// (app_settings, see SystemService.GetSettings), so the on-disk file was a
// per-container copy of the defaults that nothing read back and nothing shared
// between replicas — while still requiring a writable $HOME inside an image
// whose whole point is that it has no writable state.
//
// Behaviour is identical to what the file-backed store actually did in a
// container: every cold start created the file fresh from
// models.DefaultSettings() anyway.
func NewInMemorySettingsStore() *SettingsStore {
	return &SettingsStore{current: models.DefaultSettings()}
}

// NewSettingsStoreAt creates a SettingsStore backed by the given file path.
// An empty path yields a memory-only store (see NewInMemorySettingsStore).
// It is intended for use in tests and non-standard deployment scenarios.
func NewSettingsStoreAt(path string) (*SettingsStore, error) {
	if path == "" {
		return NewInMemorySettingsStore(), nil
	}
	s := &SettingsStore{
		path:    path,
		current: models.DefaultSettings(),
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}
	return s, nil
}

// Get returns a defensive copy of the current settings. The copy is produced by
// an explicit field/map clone (see AppSettings.Clone) rather than a JSON
// round-trip, since Get is called on hot paths (every parse, every analyze).
func (s *SettingsStore) Get() *models.AppSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current.Clone()
}

// Update replaces the stored settings. The context is accepted to satisfy
// service.SettingsProvider and is unused here: this store writes to a local
// file, with no transaction to join and no remote call to cancel.
func (s *SettingsStore) Update(_ context.Context, updated models.AppSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := migrate(&updated); err != nil {
		return err
	}
	s.current = &updated
	return s.persistLocked()
}

func (s *SettingsStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path == "" {
		return nil // memory-only store
	}

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.current = models.DefaultSettings()
		return s.persistLocked()
	}
	if err != nil {
		return err
	}

	var loaded models.AppSettings
	if err := json.Unmarshal(data, &loaded); err != nil {
		// The settings file is unparseable — most likely a partial write from
		// a crash, or a manual edit gone wrong. Quarantine it (so the operator
		// can inspect or restore) and fall back to defaults. Logging at error
		// level so the recovery is visible in ops dashboards; previously this
		// was completely silent.
		backup := s.path + ".corrupt-" + time.Now().Format("20060102T150405")
		if renameErr := os.Rename(s.path, backup); renameErr != nil {
			logger.Error("settings: failed to quarantine corrupt file",
				"path", s.path, "rename_error", renameErr, "parse_error", err)
		} else {
			logger.Error("settings: corrupt file quarantined, reset to defaults",
				"path", s.path, "backup", backup, "parse_error", err)
		}
		s.current = models.DefaultSettings()
		return s.persistLocked()
	}

	if err := migrate(&loaded); err != nil {
		return err
	}

	s.current = &loaded
	return nil
}

func (s *SettingsStore) persistLocked() error {
	if s.path == "" {
		return nil // memory-only store
	}

	data, err := json.MarshalIndent(s.current, "", "  ")
	if err != nil {
		return err
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}

	if err := os.Rename(tmp, s.path); err != nil {
		// Atomic-write failure: the .tmp file exists but the rename failed
		// (e.g. cross-device link, antivirus interference, target locked).
		// Surface this loudly — caller will return the error, but a log
		// helps post-mortem because settings persistence may be invisible
		// to UI flows.
		logger.Error("settings: atomic rename failed",
			"tmp", tmp, "target", s.path, "error", err)
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// AddRecentFile records a file/folder in the recent-files list (capped at 10).
func (s *SettingsStore) AddRecentFile(path string, size int64) error {
	return AddRecentFile(s, path, size)
}

// RemoveRecentFile removes a specific path from the recent-files list.
func (s *SettingsStore) RemoveRecentFile(path string) error {
	return RemoveRecentFile(s, path)
}

// ClearRecentFiles empties the recent-files list.
func (s *SettingsStore) ClearRecentFiles() error {
	return ClearRecentFiles(s)
}

var migrations = map[int]func(*models.AppSettings) error{}

func migrate(s *models.AppSettings) error {
	for v := s.Version + 1; v <= CurrentSettingsVersion; v++ {
		if fn, ok := migrations[v]; ok {
			if err := fn(s); err != nil {
				return fmt.Errorf("migration to v%d: %w", v, err)
			}
		}
		s.Version = v
	}
	return nil
}
