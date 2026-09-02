package main

import (
	"os"
	"path/filepath"
	"testing"
)

const mainFlowSrc = "FUNCTION Main\n    # Main entry\nEND\n"
const helperPadSrc = "FUNCTION Helper\n    # A .pad member\nEND\n"
const vendoredSrc = "FUNCTION Vendored\n    # Ignored by .bakiignore\nEND\n"

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestLoad_FolderPadAndIgnore pins R0-1: analyze/diff folder loading accepts
// .pad member files and honors .bakiignore — the same file set `fix` acts on.
// Previously the folder path was .txt-only and ignore-blind, so excluded
// files still failed the analyze gate and .pad members were invisible.
func TestLoad_FolderPadAndIgnore(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"Main.txt":    mainFlowSrc,
		"Helper.pad":  helperPadSrc,
		"Old.txt":     vendoredSrc,
		".bakiignore": "Old.txt\n",
	})

	doc, err := load(dir)
	if err != nil {
		t.Fatalf("load(folder): %v", err)
	}
	if !doc.IsFolder {
		t.Error("folder load must produce an IsFolder document")
	}
	if doc.FilePath != dir {
		t.Errorf("FilePath = %q, want the folder path", doc.FilePath)
	}
	var names []string
	for _, sf := range doc.Subflows {
		names = append(names, sf.Name)
	}
	if len(doc.Subflows) != 2 {
		t.Errorf("want exactly Main+Helper subflows (Old ignored, .pad included), got %v", names)
	}
	if len(doc.Files) != 2 {
		t.Errorf("file info count = %d, want 2", len(doc.Files))
	}
	for _, f := range doc.Files {
		if f.Name == "Old.txt" {
			t.Error("ignored file leaked into the document")
		}
	}
}

// TestLoad_FolderIgnoreDirectoryPatterns: directory-prefix patterns (vendor/)
// exclude member files like fix's expansion does.
func TestLoad_FolderIgnoreDirectoryPatterns(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"Main.txt":          mainFlowSrc,
		"vendor/Legacy.txt": vendoredSrc,
		".bakiignore":       "vendor/\n",
	})
	// vendor/ is a subdirectory — top-level expansion never reads it anyway;
	// the assertion is that loading still succeeds with the pattern present.
	doc, err := load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(doc.Subflows) != 1 || doc.Subflows[0].Name != "Main" {
		t.Errorf("want only Main, got %d subflows", len(doc.Subflows))
	}
}

// TestLoad_FolderOnlyPad: a folder with only .pad members parses (previously
// "no .txt files found").
func TestLoad_FolderOnlyPad(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"Main.pad": mainFlowSrc,
	})
	doc, err := load(dir)
	if err != nil {
		t.Fatalf("load(.pad-only folder): %v", err)
	}
	if len(doc.Subflows) != 1 || doc.Subflows[0].Name != "Main" {
		t.Errorf("want Main from Main.pad, got %+v", doc.Subflows)
	}
}

// TestLoad_FolderEmpty: no members after ignore → clear error.
func TestLoad_FolderEmpty(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"notes.md": "not a flow",
	})
	if _, err := load(dir); err == nil {
		t.Fatal("want error for folder with no flow members")
	}
}
