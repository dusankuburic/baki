package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"pad-analyzer/internal/storage/settingsdoc"
	"pad-core/logger"
	"pad-core/models"
)

// CurrentSettingsVersion is the document's own version (AppSettings.Version). It stays 1: the sections carry the
// versions that change (settingsdoc).
const CurrentSettingsVersion = 1

// sectionVersionsKey records each section's version in the settings file. A file without it, as the releases
// before section versions wrote it, holds every section at settingsdoc.LegacyVersion; those releases ignore it.
const sectionVersionsKey = "sectionVersions"

type SettingsStore struct {
	mu      sync.RWMutex
	path    string
	current *models.AppSettings
	// stored is the file's sections as last read or written, with their versions and whatever a newer binary
	// stored that this one does not know (fields, whole sections), so a write keeps it.
	stored map[string]settingsdoc.Stored
	// sections declares the document's sections; nil = settingsdoc.Default (tests swap in their own).
	sections *settingsdoc.Registry
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
	updated.Version = max(updated.Version, CurrentSettingsVersion)
	s.current = &updated
	return s.persistLocked()
}

func (s *SettingsStore) registry() *settingsdoc.Registry {
	if s.sections != nil {
		return s.sections
	}
	return settingsdoc.Default
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

	loaded, stored, err := s.decodeFile(data)
	if err != nil {
		// The settings file is unparseable (partial write, bad manual edit, a
		// section that does not decode or upgrade): quarantine it, fall back to
		// defaults, and log at error so the recovery is visible in ops dashboards.
		backup := s.path + ".corrupt-" + time.Now().Format("20060102T150405")
		if renameErr := os.Rename(s.path, backup); renameErr != nil {
			logger.Error("settings: failed to quarantine corrupt file",
				"path", s.path, "rename_error", renameErr, "parse_error", err)
		} else {
			logger.Error("settings: corrupt file quarantined, reset to defaults",
				"path", s.path, "backup", backup, "parse_error", err)
		}
		s.current = models.DefaultSettings()
		s.stored = nil
		return s.persistLocked()
	}

	s.current = loaded
	s.stored = stored
	return nil
}

// decodeFile reads the file's sections, each upgraded to the version this binary knows (settingsdoc.Decode).
func (s *SettingsStore) decodeFile(data []byte) (*models.AppSettings, map[string]settingsdoc.Stored, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, nil, err
	}
	var versions map[string]int
	if raw, ok := top[sectionVersionsKey]; ok {
		if err := json.Unmarshal(raw, &versions); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", sectionVersionsKey, err)
		}
	}
	loaded := &models.AppSettings{}
	if raw, ok := top[settingsdoc.DocVersionKey]; ok {
		if err := json.Unmarshal(raw, &loaded.Version); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", settingsdoc.DocVersionKey, err)
		}
	}
	sections := make(map[string]settingsdoc.Stored, len(top))
	for key, raw := range top {
		if key == settingsdoc.DocVersionKey || key == sectionVersionsKey {
			continue
		}
		sections[key] = settingsdoc.Stored{Version: max(versions[key], settingsdoc.LegacyVersion), Data: raw}
	}
	stored, err := s.registry().Decode(sections, loaded)
	if err != nil {
		return nil, nil, err
	}
	loaded.Version = max(loaded.Version, CurrentSettingsVersion)
	return loaded, stored, nil
}

// encodeFile lays the file out as the releases before did (the document's keys in field order, indented), with
// the sections this binary does not know after them and the section versions last.
func (s *SettingsStore) encodeFile() ([]byte, map[string]settingsdoc.Stored, error) {
	sections, err := s.registry().Encode(s.current, s.stored)
	if err != nil {
		return nil, nil, err
	}
	var unknown []string
	for key, st := range s.stored {
		if _, ok := sections[key]; !ok {
			sections[key] = st
			unknown = append(unknown, key)
		}
	}
	slices.Sort(unknown)
	versions := make(map[string]int, len(sections))
	for key, st := range sections {
		versions[key] = st.Version
	}

	var buf bytes.Buffer
	write := func(key string, value []byte) {
		if buf.Len() == 0 {
			buf.WriteByte('{')
		} else {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(key)
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(value)
	}
	for _, key := range settingsdoc.DocumentKeys(s.current) {
		if key == settingsdoc.DocVersionKey {
			v, _ := json.Marshal(s.current.Version)
			write(key, v)
		} else if st, ok := sections[key]; ok {
			write(key, st.Data)
		}
	}
	for _, key := range unknown {
		write(key, sections[key].Data)
	}
	v, err := json.Marshal(versions)
	if err != nil {
		return nil, nil, err
	}
	write(sectionVersionsKey, v)
	buf.WriteByte('}')

	var out bytes.Buffer
	if err := json.Indent(&out, buf.Bytes(), "", "  "); err != nil {
		return nil, nil, err
	}
	return out.Bytes(), sections, nil
}

// persistLocked writes the current settings durably: a unique temp file in the
// target's directory (chmod 0600), then an atomic rename. The temp name must be
// unique per write — a shared fixed "<path>.tmp" let two concurrent Updates
// interleave O_TRUNC writes and publish a mixed body. Failures remove the temp.
func (s *SettingsStore) persistLocked() error {
	if s.path == "" {
		return nil // memory-only store
	}

	data, stored, err := s.encodeFile()
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
	s.stored = stored
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
