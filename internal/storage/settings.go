package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
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

// NewSettingsStoreAt creates a SettingsStore backed by the given file path.
// It is intended for use in tests and non-standard deployment scenarios.
func NewSettingsStoreAt(path string) (*SettingsStore, error) {
	s := &SettingsStore{
		path:    path,
		current: models.DefaultSettings(),
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}
	return s, nil
}

func (s *SettingsStore) Get() *models.AppSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := json.Marshal(s.current)
	if err != nil {
		return s.current
	}
	var cp models.AppSettings
	if err := json.Unmarshal(data, &cp); err != nil {
		return s.current
	}
	return &cp
}

func (s *SettingsStore) Update(updated models.AppSettings) error {
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
