package storage

import (
	"os"
	"path/filepath"
	"testing"

	"pad-core/models"
)

// newTestStore creates a SettingsStore backed by a temp file, bypassing OS paths.
func newTestStore(t *testing.T) *SettingsStore {
	t.Helper()
	dir := t.TempDir()
	s := &SettingsStore{
		path:    filepath.Join(dir, "settings.json"),
		current: models.DefaultSettings(),
	}
	// Persist initial state so later reads see valid JSON.
	if err := s.persistLocked(); err != nil {
		t.Fatalf("newTestStore: persist: %v", err)
	}
	return s
}

func TestAddRecentFile_AddsToFront(t *testing.T) {
	s := newTestStore(t)

	if err := AddRecentFile(s, "/flow/a.txt", 100); err != nil {
		t.Fatalf("AddRecentFile: %v", err)
	}
	if err := AddRecentFile(s, "/flow/b.txt", 200); err != nil {
		t.Fatalf("AddRecentFile: %v", err)
	}

	files := s.Get().RecentFiles
	if len(files) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(files))
	}
	// Most-recently opened should be first.
	if files[0].Path != "/flow/b.txt" {
		t.Errorf("expected b.txt first, got %q", files[0].Path)
	}
	if files[1].Path != "/flow/a.txt" {
		t.Errorf("expected a.txt second, got %q", files[1].Path)
	}
}

func TestAddRecentFile_Deduplicates(t *testing.T) {
	s := newTestStore(t)

	AddRecentFile(s, "/flow/a.txt", 100)
	AddRecentFile(s, "/flow/b.txt", 200)
	AddRecentFile(s, "/flow/a.txt", 100) // re-open a.txt

	files := s.Get().RecentFiles
	if len(files) != 2 {
		t.Fatalf("expected 2 entries after dedup, got %d", len(files))
	}
	// a.txt should now be at front (most recently opened).
	if files[0].Path != "/flow/a.txt" {
		t.Errorf("expected a.txt at front after re-open, got %q", files[0].Path)
	}
}

func TestAddRecentFile_CapsAtMax(t *testing.T) {
	s := newTestStore(t)

	for i := 0; i < maxRecentFiles+5; i++ {
		path := filepath.Join(t.TempDir(), "flow.txt")
		AddRecentFile(s, path, int64(i))
	}

	files := s.Get().RecentFiles
	if len(files) > maxRecentFiles {
		t.Errorf("expected at most %d entries, got %d", maxRecentFiles, len(files))
	}
}

func TestAddRecentFile_SetsName(t *testing.T) {
	s := newTestStore(t)
	AddRecentFile(s, "/some/dir/myflow.txt", 0)

	files := s.Get().RecentFiles
	if len(files) == 0 {
		t.Fatal("expected 1 entry")
	}
	if files[0].Name != "myflow.txt" {
		t.Errorf("Name = %q, want %q", files[0].Name, "myflow.txt")
	}
}

func TestAddRecentFile_DetectsFolder(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir() // this is an actual directory on disk

	AddRecentFile(s, dir, 0)

	files := s.Get().RecentFiles
	if len(files) == 0 {
		t.Fatal("expected 1 entry")
	}
	if !files[0].IsFolder {
		t.Errorf("expected IsFolder=true for directory path")
	}
}

func TestRemoveRecentFile(t *testing.T) {
	s := newTestStore(t)
	AddRecentFile(s, "/flow/a.txt", 0)
	AddRecentFile(s, "/flow/b.txt", 0)

	if err := RemoveRecentFile(s, "/flow/a.txt"); err != nil {
		t.Fatalf("RemoveRecentFile: %v", err)
	}

	files := s.Get().RecentFiles
	if len(files) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(files))
	}
	if files[0].Path == "/flow/a.txt" {
		t.Error("removed file still present")
	}
}

func TestRemoveRecentFile_NonExistent(t *testing.T) {
	s := newTestStore(t)
	AddRecentFile(s, "/flow/a.txt", 0)

	if err := RemoveRecentFile(s, "/flow/nonexistent.txt"); err != nil {
		t.Fatalf("RemoveRecentFile on non-existent: %v", err)
	}
	if len(s.Get().RecentFiles) != 1 {
		t.Error("removing non-existent file should not change the list")
	}
}

func TestClearRecentFiles(t *testing.T) {
	s := newTestStore(t)
	AddRecentFile(s, "/a.txt", 0)
	AddRecentFile(s, "/b.txt", 0)

	if err := ClearRecentFiles(s); err != nil {
		t.Fatalf("ClearRecentFiles: %v", err)
	}
	if len(s.Get().RecentFiles) != 0 {
		t.Errorf("expected empty list after clear, got %d", len(s.Get().RecentFiles))
	}
}

func TestPurgeMissingRecentFiles(t *testing.T) {
	s := newTestStore(t)

	// Create a real temp file that exists.
	tmpFile := filepath.Join(t.TempDir(), "real.txt")
	if err := os.WriteFile(tmpFile, []byte("flow"), 0600); err != nil {
		t.Fatalf("create temp file: %v", err)
	}

	AddRecentFile(s, tmpFile, 0)
	AddRecentFile(s, "/definitely/does/not/exist.txt", 0)

	if err := PurgeMissingRecentFiles(s); err != nil {
		t.Fatalf("PurgeMissingRecentFiles: %v", err)
	}

	files := s.Get().RecentFiles
	if len(files) != 1 {
		t.Fatalf("expected 1 surviving entry, got %d", len(files))
	}
	if files[0].Path != tmpFile {
		t.Errorf("surviving entry = %q, want %q", files[0].Path, tmpFile)
	}
}

func TestPurgeMissingRecentFiles_AllMissing(t *testing.T) {
	s := newTestStore(t)
	AddRecentFile(s, "/ghost/a.txt", 0)
	AddRecentFile(s, "/ghost/b.txt", 0)

	if err := PurgeMissingRecentFiles(s); err != nil {
		t.Fatalf("PurgeMissingRecentFiles: %v", err)
	}
	if len(s.Get().RecentFiles) != 0 {
		t.Error("expected empty list after purging all missing files")
	}
}

func TestSettingsStore_GetReturnsDeepCopy(t *testing.T) {
	s := newTestStore(t)
	AddRecentFile(s, "/flow/a.txt", 0)

	got := s.Get()
	// Mutate the returned copy.
	got.RecentFiles[0].Name = "MUTATED"

	// The store's internal state must be unchanged.
	fresh := s.Get()
	if fresh.RecentFiles[0].Name == "MUTATED" {
		t.Error("Get() returned a reference to internal state rather than a deep copy")
	}
}
