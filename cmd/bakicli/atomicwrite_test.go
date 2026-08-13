package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAtomicWriteFile_ReplacesContentAndPreservesMode verifies fix --apply's
// write path: the new content lands, the original file's permission mode is
// preserved (a naked os.WriteFile over an existing file keeps its mode, so the
// temp+rename path must too), and no temp file is left behind.
func TestAtomicWriteFile_ReplacesContentAndPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flow.txt")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := atomicWriteFile(path, []byte("patched")); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "patched" {
		t.Errorf("content = %q, want %q", got, "patched")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 0644 (original mode must be preserved)", info.Mode().Perm())
	}

	// No .baki-fix-*.tmp straggler must remain in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".baki-fix-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// TestAtomicWriteFile_NewFileGetsDefaultMode verifies that writing to a path
// that does not yet exist succeeds with the 0600 default (no original mode to
// preserve).
func TestAtomicWriteFile_NewFileGetsDefaultMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	if err := atomicWriteFile(path, []byte("hello")); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("new-file mode = %o, want 0600", info.Mode().Perm())
	}
}
