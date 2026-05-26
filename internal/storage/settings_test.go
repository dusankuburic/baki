package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/models"
)

func TestSettingsStore_Update_PersistsAndGet(t *testing.T) {
	s := newTestStore(t)

	updated := *s.Get()
	updated.General.CheckForUpdates = "never"
	updated.Layout.SidebarWidth = 999

	if err := s.Update(updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got := s.Get()
	if got.General.CheckForUpdates != "never" {
		t.Errorf("CheckForUpdates = %q, want %q", got.General.CheckForUpdates, "never")
	}
	if got.Layout.SidebarWidth != 999 {
		t.Errorf("SidebarWidth = %d, want 999", got.Layout.SidebarWidth)
	}
}

func TestSettingsStore_Update_WritesToDisk(t *testing.T) {
	s := newTestStore(t)

	updated := *s.Get()
	updated.Appearance.Theme = models.ThemeLight
	if err := s.Update(updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Read a fresh store from the same path to confirm the data was persisted.
	s2 := &SettingsStore{
		path:    s.path,
		current: models.DefaultSettings(),
	}
	if err := s2.load(); err != nil {
		t.Fatalf("load after update: %v", err)
	}
	if got := s2.Get().Appearance.Theme; got != models.ThemeLight {
		t.Errorf("persisted theme = %q, want %q", got, models.ThemeLight)
	}
}

func TestSettingsStore_Load_FileNotFound_CreatesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	s := &SettingsStore{
		path:    path,
		current: models.DefaultSettings(),
	}
	if err := s.load(); err != nil {
		t.Fatalf("load on missing file: %v", err)
	}

	// File should now exist (created from defaults).
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected settings file to be created, got: %v", err)
	}

	got := s.Get()
	want := models.DefaultSettings()
	if got.Version != want.Version {
		t.Errorf("version = %d, want %d", got.Version, want.Version)
	}
}

func TestSettingsStore_Load_CorruptFile_RecoverToDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if err := os.WriteFile(path, []byte("{not valid json at all"), 0600); err != nil {
		t.Fatal(err)
	}

	s := &SettingsStore{
		path:    path,
		current: models.DefaultSettings(),
	}
	if err := s.load(); err != nil {
		t.Fatalf("expected corrupt file to be handled gracefully, got: %v", err)
	}

	// Should have fallen back to defaults.
	got := s.Get()
	want := models.DefaultSettings()
	if got.Version != want.Version {
		t.Errorf("recovered version = %d, want %d", got.Version, want.Version)
	}

	// The corrupt file should have been renamed to a backup.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var hasBackup bool
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			hasBackup = true
			break
		}
	}
	if !hasBackup {
		t.Error("expected a .corrupt-* backup file to be created alongside the corrupt settings")
	}

	// A fresh settings file should now exist at the original path.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected fresh settings file at original path after corrupt recovery, got: %v", err)
	}
}

func TestMigrate_NoOp_OnCurrentVersion(t *testing.T) {
	s := models.DefaultSettings() // Version == CurrentSettingsVersion
	before := s.Version
	if err := migrate(s); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if s.Version != before {
		t.Errorf("version changed from %d to %d; should be a no-op on current version", before, s.Version)
	}
}

func TestMigrate_AdvancesVersion(t *testing.T) {
	s := models.DefaultSettings()
	s.Version = 0 // Pretend it's an older settings file.

	if err := migrate(s); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if s.Version != CurrentSettingsVersion {
		t.Errorf("version after migrate = %d, want %d", s.Version, CurrentSettingsVersion)
	}
}

func TestSettingsStore_Get_ReturnsDeepCopy(t *testing.T) {
	s := newTestStore(t)
	original := s.Get().Appearance.Theme

	got := s.Get()
	got.Appearance.Theme = "mutated"

	// The internal state must be unchanged.
	fresh := s.Get()
	if fresh.Appearance.Theme != original {
		t.Error("Get() returned a reference to internal state rather than a deep copy")
	}
}

func TestSettingsStore_Load_NonErrNotExist_ReturnsError(t *testing.T) {
	// Point the store at a directory path — os.ReadFile on a dir is not ErrNotExist.
	dir := t.TempDir()
	s := &SettingsStore{
		path:    dir, // dir itself, not a file inside it
		current: models.DefaultSettings(),
	}
	err := s.load()
	if err == nil {
		t.Error("expected error when path is a directory, got nil")
	}
}

func TestPersistLocked_WriteFileError(t *testing.T) {
	// Use a path inside a non-existent subdirectory so WriteFile fails.
	s := &SettingsStore{
		path:    filepath.Join(t.TempDir(), "nonexistent", "settings.json"),
		current: models.DefaultSettings(),
	}
	err := s.persistLocked()
	if err == nil {
		t.Error("expected error when writing to nonexistent directory, got nil")
	}
}

func TestNewSettingsStoreAt_LoadError_ReturnsError(t *testing.T) {
	// A directory path causes load() to return a non-nil error → NewSettingsStoreAt errors.
	dir := t.TempDir()
	_, err := NewSettingsStoreAt(dir)
	if err == nil {
		t.Error("expected error when path is a directory, got nil")
	}
}

func TestMigrate_MigrationFunctionError(t *testing.T) {
	// Temporarily register a failing migration at v1 (current version), then
	// run migrate on a v0 settings object. The error path in migrate is covered.
	orig, hadOrig := migrations[1]
	migrations[1] = func(s *models.AppSettings) error {
		return errors.New("deliberate test failure")
	}
	defer func() {
		if hadOrig {
			migrations[1] = orig
		} else {
			delete(migrations, 1)
		}
	}()

	s := models.DefaultSettings()
	s.Version = 0 // triggers the v1 migration

	err := migrate(s)
	if err == nil {
		t.Error("expected error from failing migration, got nil")
	}
	if !strings.Contains(err.Error(), "migration to v1") {
		t.Errorf("error should mention migration version, got: %v", err)
	}
}
