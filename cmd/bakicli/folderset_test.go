package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sourceFiles is the set analyze reads from dir, as paths.
func sourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, k := range loadedFiles(t, dir) {
		out = append(out, filepath.Join(dir, filepath.FromSlash(k)))
	}
	return slices.Compact(out)
}

// TestExpandFixTargets_IsAnalyzesFileSet: fix and watch act on exactly the
// files analyze reads. They used to walk .git, follow symlinks and (on
// Windows) miss "vendor/" because the relative path had backslashes.
func TestExpandFixTargets_IsAnalyzesFileSet(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"Main.txt", "sub/B.pad", "vendor/Legacy.txt", ".git/Bogus.txt", "Generated.txt", "billing/nested/Deep.txt", "billing/Deep.txt"} {
		writeFlow(t, filepath.Join(dir, filepath.FromSlash(p)))
	}
	if err := os.WriteFile(filepath.Join(dir, ".bakiignore"), []byte("vendor/\r\nGenerated.txt\r\n*/Deep.txt\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeFlow(t, outside)
	if err := os.Symlink(outside, filepath.Join(dir, "leak.txt")); err != nil {
		t.Logf("symlinks unavailable (%v); the symlink case is not covered here", err)
	}

	got, err := expandFixTargets(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := sourceFiles(t, dir)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("fix set %v\nanalyze set %v", got, want)
	}
	joined := strings.Join(got, "|")
	for _, never := range []string{"Legacy", "Bogus", "Generated", "leak", filepath.Join("billing", "Deep.txt")} {
		if strings.Contains(joined, never) {
			t.Errorf("fix set contains %s: %v", never, got)
		}
	}
	// path.Match on every OS: "*" never crosses "/", so the nested file stays.
	if !strings.Contains(joined, filepath.Join("billing", "nested", "Deep.txt")) || len(got) != 3 {
		t.Errorf("want Main.txt, sub/B.pad, billing/nested/Deep.txt; got %v", got)
	}
}

// TestCLI_FixSkipsWhatAnalyzeSkips drives `bakicli fix` (dry run) over a tree:
// it proposed fixes in ignored and .git files before.
func TestCLI_FixSkipsWhatAnalyzeSkips(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"Main.txt", "vendor/Legacy.txt", ".git/Bogus.txt"} {
		path := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(cliTestFlow), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".bakiignore"), []byte("vendor/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runBakicli(t, "fix", "-config", "", "-unsafe", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, " Main.txt (line") || strings.Contains(stderr, "Legacy.txt") || strings.Contains(stderr, "Bogus.txt") {
		t.Errorf("fix touched files analyze skips (or not Main.txt):\n%s", stderr)
	}
}

// TestWarnIgnoreFile: a pattern path.Match rejects, or one with a backslash
// (a Windows habit; it is an escape, not a separator), gets a warning, once
// per folder.
func TestWarnIgnoreFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".bakiignore"), []byte("vendor\\\nbuild\\out.txt\nok/\n[\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	errPath := filepath.Join(t.TempDir(), "stderr.txt")
	f, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	warnIgnoreFile(dir)
	warnIgnoreFile(dir)
	os.Stderr = old
	_ = f.Close()
	data, _ := os.ReadFile(errPath)
	out := string(data)
	for _, want := range []string{`pattern "vendor\\" is malformed`, `pattern "build\\out.txt" has a backslash`, `pattern "[" is malformed`} {
		if strings.Count(out, want) != 1 {
			t.Errorf("want %q exactly once:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"ok/"`) {
		t.Errorf("a valid pattern was warned about:\n%s", out)
	}
}
