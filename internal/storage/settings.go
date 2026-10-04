package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// filesystem. Cloud mode uses it: deployment settings live in Postgres, so the
// on-disk file was a per-container copy of defaults nothing read back — while
// requiring a writable $HOME in an image with none.
func NewInMemorySettingsStore() *SettingsStore {
	return &SettingsStore{current: models.DefaultSettings()}
}

// NewSettingsStoreAt creates a SettingsStore backed by the given file path; an
// empty path yields a memory-only store. Intended for tests and non-standard
// deployments.
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

// Get returns a defensive copy of the current settings — an explicit field/map clone (AppSettings.Clone),
// not a JSON round-trip, since Get runs on hot paths (every parse, every analyze).
func (s *SettingsStore) Get() *models.AppSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current.Clone()
}

// Update replaces the stored settings. The context satisfies
// service.SettingsProvider only — a local file write has no transaction to
// join and no remote call to cancel.
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
		// The settings file is unparseable (partial write or bad manual edit):
		// quarantine it, fall back to defaults, and log at error so the
		// recovery is visible in ops dashboards.
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

// persistLocked writes the current settings durably: a unique temp file in the
// target's directory (chmod 0600), then an atomic rename. The temp name must be
// unique per write — a shared fixed "<path>.tmp" let two concurrent Updates
// interleave O_TRUNC writes and publish a mixed body. Failures remove the temp.
func (s *SettingsStore) persistLocked() error {
	if s.path == "" {
		return nil // memory-only store
	}

	data, err := json.MarshalIndent(s.current, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".settings-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) } // best-effort; don't mask the real error

	// CreateTemp already creates with 0600; chmod keeps that explicit and
	// in step with the other atomic writers.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		// Atomic-write failure (cross-device link, antivirus interference, target locked): log it —
		// settings persistence may be invisible to UI flows.
		logger.Error("settings: atomic rename failed",
			"tmp", tmpName, "target", s.path, "error", err)
		cleanup()
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
