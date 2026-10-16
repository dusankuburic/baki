package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
)

const miniFlow = "#Region \"Main\"\nHTTPClient.InvokeUrl Method: GET Url: $'''https://x/y'''\n#EndRegion\n"

func writeFlow(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(miniFlow), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// loadedFiles returns the SourceFile of every parsed subflow — the file keys
// loadFolder built, which is what reveals which files were actually picked up.
// (Subflow NAMES all come from the `#Region` inside the file and are identical
// across these fixtures, so they cannot distinguish anything.)
func loadedFiles(t *testing.T, dir string) []string {
	t.Helper()
	doc, err := loadFolder(dir)
	if err != nil {
		t.Fatalf("loadFolder: %v", err)
	}
	var out []string
	for i := range doc.Subflows {
		out = append(out, doc.Subflows[i].SourceFile)
	}
	sort.Strings(out)
	return out
}

// TestLoadFolder_IsRecursive is the regression gate for a folder walk that
// silently analysed almost nothing.
//
// loadFolder used os.ReadDir and skipped every directory entry, so pointing
// bakicli at a repo whose flows live in subdirectories — the normal layout —
// analysed only whatever happened to sit at the top level, reported success,
// and said nothing about the files it never opened. In CI that is a gate that
// passes because it looked at almost none of the input.
//
// shouldIgnoreFile already takes a RELATIVE PATH and already understands
// directory-prefix patterns like "vendor/", so the recursive shape was designed
// for and simply never wired up.
func TestLoadFolder_IsRecursive(t *testing.T) {
	dir := t.TempDir()
	writeFlow(t, filepath.Join(dir, "Root.txt"))
	writeFlow(t, filepath.Join(dir, "billing", "Invoice.txt"))
	writeFlow(t, filepath.Join(dir, "billing", "nested", "Deep.txt"))
	writeFlow(t, filepath.Join(dir, "hr", "Onboard.pad"))

	got := loadedFiles(t, dir)
	if len(got) != 4 {
		t.Errorf("found %d flows %v, want 4 — files in subdirectories were silently skipped", len(got), got)
	}
}

// TestLoadFolder_KeysDisambiguateSameNamedFiles guards the thing recursion
// breaks if keys stay bare basenames: two flows called Main.txt in different
// folders must not collide into one.
func TestLoadFolder_KeysDisambiguateSameNamedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFlow(t, filepath.Join(dir, "teamA", "Main.txt"))
	writeFlow(t, filepath.Join(dir, "teamB", "Main.txt"))

	got := loadedFiles(t, dir)
	if len(got) != 2 {
		t.Errorf("two same-named flows in different folders collapsed to %d (%v) — one team's "+
			"flow silently replaced the other's", len(got), got)
	}
}

// TestLoadFolder_HonoursBakiignoreInSubdirectories keeps the ignore contract
// working once paths are relative rather than bare names.
func TestLoadFolder_HonoursBakiignoreInSubdirectories(t *testing.T) {
	dir := t.TempDir()
	writeFlow(t, filepath.Join(dir, "Keep.txt"))
	writeFlow(t, filepath.Join(dir, "vendor", "Skip.txt"))
	writeFlow(t, filepath.Join(dir, "Generated.txt"))
	if err := os.WriteFile(filepath.Join(dir, ".bakiignore"),
		[]byte("vendor/\nGenerated.txt\n"), 0o600); err != nil {
		t.Fatalf("write .bakiignore: %v", err)
	}

	got := loadedFiles(t, dir)
	joined := strings.Join(got, ",")
	if strings.Contains(joined, "Skip") {
		t.Errorf("a directory-prefix ignore pattern (vendor/) did not apply to a nested file: %v", got)
	}
	if strings.Contains(joined, "Generated") {
		t.Errorf("a filename ignore pattern did not apply: %v", got)
	}
	if !strings.Contains(joined, "Keep") {
		t.Errorf("the ignore patterns excluded a file they should not have: %v", got)
	}
}

// TestLoadFolder_SkipsDotDirectories keeps a recursive walk from descending
// into .git and friends, which would be slow and meaningless.
func TestLoadFolder_SkipsDotDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFlow(t, filepath.Join(dir, "Real.txt"))
	writeFlow(t, filepath.Join(dir, ".git", "Bogus.txt"))

	got := loadedFiles(t, dir)
	if len(got) != 1 {
		t.Errorf("walked into a dot-directory: %v", got)
	}
}

// TestCLI_SymlinkedRootFolder: a folder named through a symlink is analyzed
// (it listed nothing: "no .txt or .pad files found", exit 2), and fix and watch
// get its files; a symlink inside it is still skipped.
func TestCLI_SymlinkedRootFolder(t *testing.T) {
	target := t.TempDir()
	writeFlow(t, filepath.Join(target, "sub", "Real.txt"))
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeFlow(t, outside)
	link := filepath.Join(t.TempDir(), "flows")
	for _, l := range [][2]string{{target, link}, {outside, filepath.Join(target, "leak.txt")}} {
		if err := os.Symlink(l[0], l[1]); err != nil {
			const errPrivilegeNotHeld = syscall.Errno(1314) // ERROR_PRIVILEGE_NOT_HELD
			if runtime.GOOS == "windows" && errors.Is(err, errPrivilegeNotHeld) {
				t.Skipf("this process may not create symlinks (enable Developer Mode or run elevated): %v", err)
			}
			t.Fatal(err)
		}
	}

	if got := loadedFiles(t, link); !slices.Equal(got, []string{"sub/Real.txt"}) {
		t.Errorf("loadFolder through the link read %v, want sub/Real.txt", got)
	}
	if files, err := expandFixTargets(link); err != nil || !slices.Equal(files, []string{filepath.Join(link, "sub", "Real.txt")}) {
		t.Errorf("expandFixTargets through the link = %v, %v", files, err)
	}
	// miniFlow's hard-coded URL is an info finding.
	stdout, stderr, code := runBakicli(t, "-config", "", "-format", "json", "-fail-on", "error", link)
	if code != 0 || !strings.Contains(stdout, `"ruleId": "hardcoded-url"`) {
		t.Errorf("bakicli on a symlinked folder: exit %d\nstdout: %.300s\nstderr: %s", code, stdout, stderr)
	}
}

// TestLoadFolder_DoesNotFollowSymlinksOutOfTheTree is a security gate, not a
// tidiness one.
//
// bakicli runs on untrusted content in CI — the GitHub Action analyses whatever
// a pull request contains — and ParseError.Snippet embeds VERBATIM SOURCE into
// the SARIF that is uploaded to GitHub code scanning. So a PR that commits
// `flows/leak.txt -> /etc/passwd`, or a link to a CI secret file, would have had
// that file parsed and its contents published. Reads are root-scoped via
// os.OpenRoot and symlinks are skipped outright.
func TestLoadFolder_DoesNotFollowSymlinksOutOfTheTree(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOP_SECRET_CONTENT_MUST_NOT_BE_READ\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	dir := t.TempDir()
	writeFlow(t, filepath.Join(dir, "Real.txt"))
	if err := os.Symlink(outside, filepath.Join(dir, "leak.txt")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	doc, err := loadFolder(dir)
	if err != nil {
		t.Fatalf("loadFolder: %v", err)
	}
	for i := range doc.Subflows {
		if strings.Contains(doc.Subflows[i].SourceFile, "leak") {
			t.Errorf("followed a symlink out of the scanned tree: %s", doc.Subflows[i].SourceFile)
		}
	}
	if strings.Contains(doc.Source, "TOP_SECRET") {
		t.Error("the contents of a file outside the scanned folder were read into the document")
	}
}
