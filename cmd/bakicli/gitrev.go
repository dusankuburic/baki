package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pad-core/parser"
)

// gitCmdTimeout bounds every git invocation. A hung git (a prompting credential
// helper, a network remote in a rev spec) must not wedge a CI job forever.
const gitCmdTimeout = 30 * time.Second

// errNotInGitRepo is returned when the target is not inside a work tree, so the
// caller can explain that rather than surface a raw git error.
var errNotInGitRepo = fmt.Errorf("not inside a git repository")

// validateRev rejects revision strings that git would interpret as OPTIONS.
//
// The rev reaches exec.Command as an argument, so there is no shell to inject
// into — but git itself parses leading-dash arguments as flags, and several
// (--upload-pack, --exec) run a command of the attacker's choosing. A rev
// comes from CI configuration, not always more trusted than the repo, so it
// is validated rather than assumed benign.
func validateRev(rev string) error {
	if rev == "" {
		return fmt.Errorf("empty revision")
	}
	if strings.HasPrefix(rev, "-") {
		return fmt.Errorf("invalid revision %q: must not begin with '-'", rev)
	}
	if strings.ContainsAny(rev, "\x00\n") {
		return fmt.Errorf("invalid revision %q: contains a control character", rev)
	}
	return nil
}

// runGit executes git in dir and returns stdout.
func runGit(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()
	// #nosec G204 -- the executable is the fixed literal "git", never caller
	// supplied, and no shell is involved (exec.Command does not interpret
	// metacharacters). The only non-literal arguments are a revision, which
	// validateRev has already rejected if it begins with '-' or carries a
	// control character, and a repository-relative path derived from the
	// filesystem walk. The leading-dash check is what makes this safe — git
	// parses such arguments as OPTIONS, and --upload-pack/--exec run a command
	// of the caller's choosing; TestValidateRev_RejectsOptionLikeRevisions
	// fails if that guard is removed.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Never let git prompt: in CI a prompt is an indefinite hang, and locally it
	// is a confusing stall in the middle of an analysis run.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git %s timed out after %s", strings.Join(args, " "), gitCmdTimeout)
		}
		return nil, err
	}
	return out, nil
}

// gitRepoRoot returns the work-tree root containing path.
func gitRepoRoot(path string) (string, error) {
	dir := path
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		dir = filepath.Dir(path)
	}
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errNotInGitRepo
	}
	return strings.TrimSpace(string(out)), nil
}

// gitFileAtRev returns the contents of repo-relative relPath as of rev.
//
// ok is false when the file did not exist at that revision — not an error:
// a newly added flow has no prior findings, so every finding in it is new.
func gitFileAtRev(root, rev, relPath string) (content string, ok bool, err error) {
	if err := validateRev(rev); err != nil {
		return "", false, err
	}
	// No `--` here: `git show` takes an OBJECT spec, not a pathspec, and adding
	// the separator makes git ignore the spec and print the HEAD commit instead
	// — silently returning a commit message for every file and swallowing bad
	// revisions. validateRev guarantees no leading dash, so "rev:path" can
	// never be parsed as an option.
	out, gerr := runGit(root, "show", rev+":"+filepath.ToSlash(relPath))
	if gerr != nil {
		// git exits non-zero both for "no such path at rev" and a bad rev;
		// distinguish them so a typo'd branch is reported instead of silently
		// making every finding look new.
		if _, revErr := runGit(root, "rev-parse", "--verify", rev+"^{commit}"); revErr != nil {
			return "", false, fmt.Errorf("unknown revision %q", rev)
		}
		return "", false, nil
	}
	return string(out), true, nil
}

// flowFilesAtRev rebuilds the analysed file set as it existed at rev: it walks
// the CURRENT target to decide which files matter, then reads each one's
// historical content. Files absent at rev are skipped — a newly added flow
// has no prior findings, so everything in it is legitimately new.
func flowFilesAtRev(target, rev string) (map[string]string, error) {
	root, err := gitRepoRoot(target)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}

	// The keys ParseFiles sees must match the ones the live analysis used, or
	// fingerprints (which fold in the subflow name) will not line up and every
	// finding would look new.
	live := map[string]string{}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		keys, lerr := parser.ListFolder(abs, parser.TreeFolder())
		if lerr != nil {
			return nil, lerr
		}
		for _, k := range keys {
			live[k] = ""
		}
	} else {
		live[filepath.Base(abs)] = ""
	}

	base := abs
	if !info.IsDir() {
		base = filepath.Dir(abs)
	}

	out := make(map[string]string, len(live))
	for name := range live {
		onDisk := filepath.Join(base, filepath.FromSlash(name))
		rel, rerr := filepath.Rel(root, onDisk)
		if rerr != nil {
			continue
		}
		content, ok, gerr := gitFileAtRev(root, rev, rel)
		if gerr != nil {
			return nil, gerr
		}
		if !ok {
			continue // added since rev — all of its findings are new
		}
		out[name] = content
	}
	return out, nil
}
