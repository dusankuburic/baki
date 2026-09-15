package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newGitRepo builds a throwaway repository with one committed flow and returns
// its root. Skips when git is unavailable rather than failing the suite.
func newGitRepo(t *testing.T, committed map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v (%s)", args, err, out)
		}
	}
	for name, content := range committed {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v (%s)", args, err, out)
		}
	}
	return root
}

// TestValidateRev_RejectsOptionLikeRevisions guards the one place user input
// reaches exec.
//
// There is no shell here, so there is nothing to inject into — but git itself
// parses leading-dash arguments as OPTIONS, and several of them
// (--upload-pack, --exec) run a command of the caller's choosing. A revision
// comes from CI configuration, which is not automatically more trusted than the
// repository it is analysing.
func TestValidateRev_RejectsOptionLikeRevisions(t *testing.T) {
	for _, bad := range []string{"", "-x", "--upload-pack=/bin/sh", "--exec=evil", "main\nHEAD", "a\x00b"} {
		if err := validateRev(bad); err == nil {
			t.Errorf("validateRev(%q) accepted an unsafe revision", bad)
		}
	}
	for _, good := range []string{"HEAD", "origin/main", "v1.2.3", "abc1234", "HEAD~3"} {
		if err := validateRev(good); err != nil {
			t.Errorf("validateRev(%q) rejected a legitimate revision: %v", good, err)
		}
	}
}

// TestGitFileAtRev_DistinguishesMissingPathFromBadRevision is the correctness
// gate for the comparison's foundation.
//
// These two cases must NOT look alike. A path absent at the revision means "this
// flow is new, so all of its findings are new" — a normal, correct outcome. An
// unknown revision means the caller typo'd a branch, and silently treating that
// as an empty baseline would either flag everything (and get the gate switched
// off) or nothing (and let real regressions through).
//
// This also pins the git invocation itself: `git show` takes an OBJECT spec, not
// a pathspec, so adding a `--` separator makes git ignore the spec and print the
// HEAD commit instead. That bug returned a commit message for every file,
// reported missing paths as present, and swallowed bad revisions entirely.
func TestGitFileAtRev_DistinguishesMissingPathFromBadRevision(t *testing.T) {
	root := newGitRepo(t, map[string]string{"flows/Main.txt": miniFlow})

	content, ok, err := gitFileAtRev(root, "HEAD", "flows/Main.txt")
	if err != nil || !ok {
		t.Fatalf("committed file at HEAD: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(content, "HTTPClient") {
		t.Errorf("got %q — that is not the file's content (a `--` separator makes git show "+
			"print the commit instead)", content)
	}

	if _, ok, err := gitFileAtRev(root, "HEAD", "flows/NotThere.txt"); err != nil || ok {
		t.Errorf("an absent path should be (ok=false, nil error); got ok=%v err=%v", ok, err)
	}

	if _, _, err := gitFileAtRev(root, "definitely-not-a-branch", "flows/Main.txt"); err == nil {
		t.Error("an unknown revision must be an error — silently treating it as an empty " +
			"baseline either flags everything or catches nothing")
	}
}

// TestFlowFilesAtRev_ReconstructsTheHistoricalSet covers the reconstruction the
// gate compares against, including a file added since the revision.
func TestFlowFilesAtRev_ReconstructsTheHistoricalSet(t *testing.T) {
	root := newGitRepo(t, map[string]string{"flows/Main.txt": miniFlow})
	flows := filepath.Join(root, "flows")

	// Add a file that did not exist at HEAD.
	writeFlow(t, filepath.Join(flows, "Added.txt"))

	got, err := flowFilesAtRev(flows, "HEAD")
	if err != nil {
		t.Fatalf("flowFilesAtRev: %v", err)
	}
	if _, ok := got["Main.txt"]; !ok {
		t.Errorf("the committed flow is missing from the historical set: %v", keysOf(got))
	}
	if _, ok := got["Added.txt"]; ok {
		t.Errorf("a file added since the revision was included in the historical set — its "+
			"findings would be treated as pre-existing: %v", keysOf(got))
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestGitRepoRoot_ReportsNonRepos keeps the error message actionable rather than
// leaking a raw git failure.
func TestGitRepoRoot_ReportsNonRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := gitRepoRoot(t.TempDir()); err == nil {
		t.Error("a directory outside any work tree should report that it is not a git repository")
	}
}
