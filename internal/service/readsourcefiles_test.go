package service

import (
	"os"
	"path/filepath"
	"testing"

	"pad-core/models"
)

// TestReadSourceFiles_SkipsInvalidNames verifies S4: ReadSourceFiles runs the
// shared validateUserPath guard, so empty / null-byte filenames are skipped
// before touching the filesystem, while valid siblings are still read.
func TestReadSourceFiles_SkipsInvalidNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	svc := &FlowService{}
	doc := &models.FlowDocument{FilePath: filepath.Join(dir, "flow.txt")}

	res, err := svc.ReadSourceFiles(doc, []string{"real.txt", "", "bad\x00name"})
	if err != nil {
		t.Fatalf("ReadSourceFiles: %v", err)
	}

	if got := res["real.txt"]; got != "hello" {
		t.Errorf("valid file: got %q, want %q", got, "hello")
	}
	if _, ok := res[""]; ok {
		t.Error("empty filename should have been skipped by validateUserPath")
	}
	if _, ok := res["bad\x00name"]; ok {
		t.Error("null-byte filename should have been skipped by validateUserPath")
	}
	if len(res) != 1 {
		t.Errorf("expected exactly 1 result, got %d: %v", len(res), res)
	}
}

// TestReadSourceFiles_NilDoc verifies the nil-document guard returns cleanly.
func TestReadSourceFiles_NilDoc(t *testing.T) {
	svc := &FlowService{}
	res, err := svc.ReadSourceFiles(nil, []string{"x.txt"})
	if err != nil || res != nil {
		t.Errorf("nil doc: got (res=%v, err=%v), want (nil, nil)", res, err)
	}
}

// TestReadSourceFiles_ConfinesToFolder: filenames must stay single path
// components of the opened flow folder. The cloud provider strips FilePath so
// this is defense-in-depth server-side, but a ".." escape must never depend on
// an invariant two call-sites away (PatchFlow applies the same guard).
func TestReadSourceFiles_ConfinesToFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	outside := filepath.Join(dir, "..", "secret-outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside fixture: %v", err)
	}

	svc := &FlowService{}
	doc := &models.FlowDocument{FilePath: filepath.Join(dir, "flow.txt")}

	res, err := svc.ReadSourceFiles(doc, []string{"real.txt", "../secret-outside.txt", `sub\escape.txt`})
	if err != nil {
		t.Fatalf("ReadSourceFiles: %v", err)
	}
	if got := res["real.txt"]; got != "inside" {
		t.Errorf("sibling file: got %q, want %q", got, "inside")
	}
	if len(res) != 1 {
		t.Errorf("traversal attempts must be skipped, got %d results: %v", len(res), res)
	}
}
