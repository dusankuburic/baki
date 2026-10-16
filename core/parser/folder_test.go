package parser

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// writeFolder creates files (slash keys) under a new temp dir.
func writeFolder(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for key, text := range files {
		p := filepath.Join(dir, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// symlinkOrSkip links link -> target, skipping where Windows refuses this
// process the privilege (neither Developer Mode nor an elevated token).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		const errPrivilegeNotHeld = syscall.Errno(1314) // ERROR_PRIVILEGE_NOT_HELD
		if runtime.GOOS == "windows" && errors.Is(err, errPrivilegeNotHeld) {
			t.Skipf("this process may not create symlinks (enable Developer Mode or run elevated): %v", err)
		}
		t.Fatalf("symlink: %v", err)
	}
}

const memberFlow = "FUNCTION Main\n    # a member\nEND\n"

// folderTree has a member at every interesting place: upper-case and .pad
// extensions, a dot file, dot directories, an ignored file and directory, a
// directory with a member's suffix, same-named files in two directories.
var folderTree = map[string]string{
	"Root.txt":                 memberFlow,
	"Upper.TXT":                memberFlow,
	"Helper.PAD":               memberFlow,
	".hidden.txt":              memberFlow,
	"notes.md":                 "not a flow",
	"Generated.txt":            memberFlow,
	"billing/Invoice.txt":      memberFlow,
	"billing/nested/Deep.txt":  memberFlow,
	"hr/Onboard.pad":           memberFlow,
	"hr/readme.md":             "not a flow",
	"teamA/Main.txt":           memberFlow,
	"teamB/Main.txt":           memberFlow,
	"vendor/Legacy.txt":        memberFlow,
	".git/Bogus.txt":           memberFlow,
	".github/workflows/ci.txt": memberFlow,
	"dir.txt/Inner.txt":        memberFlow,
	".bakiignore":              "vendor/\nGenerated.txt\n",
}

func TestListFolder(t *testing.T) {
	dir := writeFolder(t, folderTree)
	tree := TreeFolder()
	noIgnore := TreeFolder()
	noIgnore.IgnoreFile = ""
	dotDirs := TreeFolder()
	dotDirs.SkipDotDirs = false
	padOnly := TreeFolder()
	padOnly.Extensions = []string{".pad"}

	treeKeys := []string{".hidden.txt", "Helper.PAD", "Root.txt", "Upper.TXT", "billing/Invoice.txt",
		"billing/nested/Deep.txt", "dir.txt/Inner.txt", "hr/Onboard.pad", "teamA/Main.txt", "teamB/Main.txt"}
	cases := []struct {
		name string
		opts FolderOptions
		want []string
	}{
		{"tree", tree, treeKeys},
		{"tree without ignore file", noIgnore, sortedWith(treeKeys, "Generated.txt", "vendor/Legacy.txt")},
		{"tree with dot dirs", dotDirs, sortedWith(treeKeys, ".git/Bogus.txt", ".github/workflows/ci.txt")},
		{"tree .pad only", padOnly, []string{"Helper.PAD", "hr/Onboard.pad"}},
		{"export folder", ExportFolder(), []string{".hidden.txt", "Generated.txt", "Root.txt", "Upper.TXT"}},
		{"zero options", FolderOptions{}, []string{".hidden.txt", "Generated.txt", "Helper.PAD", "Root.txt", "Upper.TXT"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ListFolder(dir, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ListFolder =\n  %v\nwant\n  %v", got, tc.want)
			}
		})
	}
}

func sortedWith(keys []string, more ...string) []string {
	out := append(slices.Clone(keys), more...)
	slices.Sort(out)
	return out
}

// TestListFolder_IgnoreFiles runs ignore files through a real walk: directory
// pruning, OS-independent wildcards, and files written on Windows.
func TestListFolder_IgnoreFiles(t *testing.T) {
	files := map[string]string{
		"Main.txt":                memberFlow,
		"Generated.txt":           memberFlow,
		"billing/Invoice.txt":     memberFlow,
		"billing/nested/Deep.txt": memberFlow,
		"nested/Deep.txt":         memberFlow,
		"vendor/Legacy.txt":       memberFlow,
		"vendor/sub/Old.pad":      memberFlow,
	}
	all := []string{"Generated.txt", "Main.txt", "billing/Invoice.txt", "billing/nested/Deep.txt",
		"nested/Deep.txt", "vendor/Legacy.txt", "vendor/sub/Old.pad"}
	cases := []struct {
		name, ignore string
		want         []string
	}{
		{"directory prefix", "vendor/\n", without(all, "vendor/Legacy.txt", "vendor/sub/Old.pad")},
		{"bare directory name prunes it", "vendor\n", without(all, "vendor/Legacy.txt", "vendor/sub/Old.pad")},
		{"base name anywhere", "Deep.txt\n", without(all, "billing/nested/Deep.txt", "nested/Deep.txt")},
		{"extension glob", "*.pad\n", without(all, "vendor/sub/Old.pad")},
		// billing/* matches "billing/" (* matches the empty name), so the walk
		// prunes the whole billing directory: the same on every OS.
		{"one level glob", "billing/*\n", without(all, "billing/Invoice.txt", "billing/nested/Deep.txt")},
		// Only the directory's own rel path matches a multi-segment pattern
		// without the slash ("billing/nested/" and "nested" do not).
		{"nested directory path", "billing/nested\n", without(all, "billing/nested/Deep.txt")},
		// * never crosses "/": filepath.Match on Windows let it, so this
		// pattern also dropped billing/nested/Deep.txt there.
		{"star stops at slash", "*/Deep.txt\n", without(all, "nested/Deep.txt")},
		{"comments and blanks", "# vendored\n\n   \nGenerated.txt\n", without(all, "Generated.txt")},
		{"CRLF and BOM", "\ufeffvendor/\r\nGenerated.txt\r\n",
			without(all, "Generated.txt", "vendor/Legacy.txt", "vendor/sub/Old.pad")},
		// A backslash escapes, as in path.Match: Windows-style patterns match
		// nothing (they never matched in analyze, on any OS).
		{"backslash directory", "vendor\\\n", all},
		{"backslash glob", "vendor\\*\n", all},
		{"case-sensitive", "VENDOR/\n", all},
		{"empty file", "", all},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree := map[string]string{".bakiignore": tc.ignore}
			for k, v := range files {
				tree[k] = v
			}
			got, err := ListFolder(writeFolder(t, tree), TreeFolder())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf(".bakiignore %q:\n  got  %v\n  want %v", tc.ignore, got, tc.want)
			}
		})
	}
}

func without(keys []string, drop ...string) []string {
	return slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return slices.Contains(drop, k) })
}

func TestIgnoreList_Match(t *testing.T) {
	cases := []struct {
		patterns []string
		rel      string
		want     bool
	}{
		// Moved from cmd/bakicli (shouldIgnoreFile).
		{[]string{"*.tmp", "vendor/*", "samples/"}, "foo.tmp", true},
		{[]string{"*.tmp", "vendor/*", "samples/"}, "dir/foo.tmp", true},
		{[]string{"*.tmp", "vendor/*", "samples/"}, "vendor/flow.txt", true},
		{[]string{"*.tmp", "vendor/*", "samples/"}, "real.txt", false},
		{[]string{"*.tmp", "vendor/*", "samples/"}, "samples/x.txt", true},
		{[]string{"*.tmp", "vendor/*", "samples/"}, "src/main.txt", false},
		{nil, "anything.txt", false},
		{[]string{}, "anything.txt", false},

		{[]string{"vendor/"}, "vendor/", true},
		{[]string{"vendor/"}, "vendor", false},
		{[]string{"vendor"}, "vendor", true},
		{[]string{"vendor"}, "vendor/x.txt", false},
		{[]string{"vendor/*"}, "vendor/nested/x.txt", false},
		{[]string{"vendor/*"}, "vendor/nested", true},
		{[]string{"billing/*"}, "billing/nested/Deep.txt", false},
		{[]string{"*/Deep.txt"}, "billing/nested/Deep.txt", false},
		{[]string{"*/Deep.txt"}, "nested/Deep.txt", true},
		{[]string{"[ab]*.txt"}, "x/b1.txt", true},
		{[]string{"[ab"}, "a", false}, // malformed: matches nothing
		{[]string{`\*.txt`}, "*.txt", true},
		{[]string{`\*.txt`}, "a.txt", false},
		{[]string{`vendor\`}, "vendor/x.txt", false},
		{[]string{`vendor\*`}, "vendor/x.txt", false},
		{[]string{"Vendor/"}, "vendor/x.txt", false},
		// rel is a slash path; a backslash in it is part of a name.
		{[]string{"vendor/"}, `vendor\x.txt`, false},
	}
	for _, tc := range cases {
		if got := IgnoreList(tc.patterns).Match(tc.rel); got != tc.want {
			t.Errorf("%q.Match(%q) = %t, want %t", tc.patterns, tc.rel, got, tc.want)
		}
	}
}

func TestParseIgnoreList(t *testing.T) {
	cases := []struct {
		text string
		want IgnoreList
	}{
		{"# comment\n*.tmp\nvendor/\n\n", IgnoreList{"*.tmp", "vendor/"}},
		{"\ufeffvendor/\r\n  *.bak  \r\n# x\r\n", IgnoreList{"vendor/", "*.bak"}},
		{"\tvendor/\t\n", IgnoreList{"vendor/"}},
		{"", nil},
		{"# only\n\n", nil},
	}
	for _, tc := range cases {
		if got := ParseIgnoreList(tc.text); !slices.Equal(got, tc.want) {
			t.Errorf("ParseIgnoreList(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestReadIgnoreFile(t *testing.T) {
	dir := writeFolder(t, map[string]string{".bakiignore": "# comment\n*.tmp\nvendor/\n\n"})
	if got := ReadIgnoreFile(dir, ".bakiignore"); !slices.Equal(got, IgnoreList{"*.tmp", "vendor/"}) {
		t.Errorf("ReadIgnoreFile = %q", got)
	}
	if got := ReadIgnoreFile(dir, ".missing"); got != nil {
		t.Errorf("a missing ignore file gave %q, want nil", got)
	}
}

func TestReadFolder(t *testing.T) {
	dir := writeFolder(t, map[string]string{
		"Main.txt":          "main text",
		"teamA/Sub.pad":     "sub text",
		"vendor/Legacy.txt": "ignored",
		".bakiignore":       "vendor/\n",
	})
	got, err := ReadFolder(dir, TreeFolder())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Main.txt": "main text", "teamA/Sub.pad": "sub text"}
	if len(got) != len(want) {
		t.Fatalf("ReadFolder = %q, want %q", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ReadFolder[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestReadFolder_ConfinedSkipsSymlinks is a security gate: bakicli runs on
// pull-request content in CI and a parse error's snippet is verbatim source in
// the uploaded SARIF, so a committed `leak.txt -> /etc/passwd` must not be read.
func TestReadFolder_ConfinedSkipsSymlinks(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "Deep.txt"), []byte("TOP_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := writeFolder(t, map[string]string{"Real.txt": memberFlow})
	symlinkOrSkip(t, secret, filepath.Join(dir, "leak.txt"))
	symlinkOrSkip(t, outside, filepath.Join(dir, "linked"))

	got, err := ReadFolder(dir, TreeFolder())
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range got {
		if k != "Real.txt" || strings.Contains(v, "TOP_SECRET") {
			t.Errorf("read %q outside the folder", k)
		}
	}

	// A recursive walk never follows a symlinked directory, confined or not;
	// an unconfined one reads a symlinked file.
	loose := TreeFolder()
	loose.Confined = false
	keys, err := ListFolder(dir, loose)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Real.txt", "leak.txt"}; !slices.Equal(keys, want) {
		t.Errorf("unconfined ListFolder = %v, want %v", keys, want)
	}
}

func TestReadFolder_MaxFileSize(t *testing.T) {
	dir := writeFolder(t, map[string]string{"Main.txt": "small", "big/Big.txt": strings.Repeat("x", 100)})
	opts := TreeFolder()
	opts.MaxFileSize = 50
	_, err := ReadFolder(dir, opts)
	var tooLarge *FileTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want *FileTooLargeError", err)
	}
	if *tooLarge != (FileTooLargeError{Name: "big/Big.txt", Size: 100, Max: 50}) {
		t.Errorf("FileTooLargeError = %+v", *tooLarge)
	}
	if _, err := ParseFolderTree(dir, opts); !errors.As(err, &tooLarge) {
		t.Errorf("ParseFolderTree: err = %v, want *FileTooLargeError", err)
	}

	opts.MaxFileSize = 100
	if got, err := ReadFolder(dir, opts); err != nil || len(got["big/Big.txt"]) != 100 {
		t.Errorf("a member exactly at the limit: %v", err)
	}
}

// ListFolder with MaxFileSize is the desktop's stat-only pre-check: it fails
// before any member is read, and only members count.
func TestListFolder_MaxFileSize(t *testing.T) {
	dir := writeFolder(t, map[string]string{
		"Main.txt": "small", "Big.txt": strings.Repeat("x", 100), "notes.md": strings.Repeat("x", 500),
		"sub/Deep.txt": strings.Repeat("x", 80),
	})
	cases := []struct {
		name string
		opts FolderOptions
		max  int64
		want *FileTooLargeError // nil: listed
	}{
		{"flat over", ExportFolder(), 50, &FileTooLargeError{Name: "Big.txt", Size: 100, Max: 50}},
		{"flat at the limit", ExportFolder(), 100, nil},
		{"tree over", TreeFolder(), 90, &FileTooLargeError{Name: "Big.txt", Size: 100, Max: 90}},
		{"tree at the limit", TreeFolder(), 100, nil},
		{"no limit", TreeFolder(), 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.MaxFileSize = tc.max
			keys, err := ListFolder(dir, opts)
			var tooLarge *FileTooLargeError
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("err = %v", err)
			case tc.want == nil && len(keys) < 2:
				t.Fatalf("keys = %v", keys)
			case tc.want != nil && !errors.As(err, &tooLarge):
				t.Fatalf("err = %v, want *FileTooLargeError", err)
			case tc.want != nil && *tooLarge != *tc.want:
				t.Errorf("FileTooLargeError = %+v, want %+v", *tooLarge, *tc.want)
			}
		})
	}

	nested := writeFolder(t, map[string]string{"Main.txt": "small", "sub/Deep.txt": strings.Repeat("x", 80)})
	opts := TreeFolder()
	opts.MaxFileSize = 79
	var tooLarge *FileTooLargeError
	if _, err := ListFolder(nested, opts); !errors.As(err, &tooLarge) || tooLarge.Name != "sub/Deep.txt" {
		t.Errorf("nested member over the limit: err = %v", err)
	}

	t.Run("followed symlink counts its target", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "Huge.txt")
		if err := os.WriteFile(target, []byte(strings.Repeat("x", 200)), 0o600); err != nil {
			t.Fatal(err)
		}
		linkDir := writeFolder(t, map[string]string{"Main.txt": "small"})
		symlinkOrSkip(t, target, filepath.Join(linkDir, "Linked.txt"))
		opts := ExportFolder()
		opts.MaxFileSize = 100
		var tooLarge *FileTooLargeError
		if _, err := ListFolder(linkDir, opts); !errors.As(err, &tooLarge) || tooLarge.Size != 200 {
			t.Errorf("err = %v, want *FileTooLargeError for the 200-byte target", err)
		}
	})
}

func TestReadFolder_Errors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	cases := []struct {
		name   string
		opts   FolderOptions
		prefix string
	}{
		{"confined", TreeFolder(), "open folder: "},
		{"recursive", FolderOptions{Recursive: true}, "read folder: "},
		{"flat", ExportFolder(), "read folder: "},
	}
	for _, tc := range cases {
		if _, err := ReadFolder(missing, tc.opts); err == nil || !strings.HasPrefix(err.Error(), tc.prefix) {
			t.Errorf("%s: err = %v, want prefix %q", tc.name, err, tc.prefix)
		}
		if _, err := ListFolder(missing, tc.opts); err == nil || !strings.HasPrefix(err.Error(), "read folder: ") {
			t.Errorf("%s: ListFolder err = %v", tc.name, err)
		}
	}

	file := filepath.Join(writeFolder(t, map[string]string{"Main.txt": memberFlow}), "Main.txt")
	for _, opts := range []FolderOptions{{Recursive: true}, ExportFolder()} {
		if keys, err := ListFolder(file, opts); err == nil {
			t.Errorf("ListFolder of a file (recursive %t) = %v, want an error", opts.Recursive, keys)
		}
	}
}

// A symlinked folder named as the root is followed, as the flat desktop listing
// and the confined read's os.OpenRoot always did (the recursive walk listed
// nothing: WalkDir does not follow its root). Inside it the tree's policy
// holds: a symlinked directory is never walked (no loop through a link back to
// the root) and a confined walk skips a symlinked member.
func TestListFolder_SymlinkedRoot(t *testing.T) {
	outside := writeFolder(t, map[string]string{"secret.txt": "TOP_SECRET"})
	target := writeFolder(t, map[string]string{"Main.txt": memberFlow, "sub/Deep.txt": memberFlow})
	symlinkOrSkip(t, target, filepath.Join(target, "loop"))
	symlinkOrSkip(t, filepath.Join(outside, "secret.txt"), filepath.Join(target, "leak.txt"))
	link := filepath.Join(t.TempDir(), "flows")
	symlinkOrSkip(t, target, link)
	// A link to the link resolves to the same folder.
	chained := filepath.Join(t.TempDir(), "again")
	symlinkOrSkip(t, link, chained)

	for _, root := range []string{link, chained} {
		if keys, err := ListFolder(root, TreeFolder()); err != nil || !slices.Equal(keys, []string{"Main.txt", "sub/Deep.txt"}) {
			t.Errorf("recursive ListFolder of the symlinked root %s = %v, %v", filepath.Base(root), keys, err)
		}
		files, err := ReadFolder(root, TreeFolder())
		if err != nil || len(files) != 2 || files["sub/Deep.txt"] != memberFlow {
			t.Errorf("ReadFolder of the symlinked root %s = %v keys, %v", filepath.Base(root), len(files), err)
		}
		loose := TreeFolder()
		loose.Confined = false
		if keys, err := ListFolder(root, loose); err != nil || !slices.Equal(keys, []string{"Main.txt", "leak.txt", "sub/Deep.txt"}) {
			t.Errorf("unconfined ListFolder of the symlinked root %s = %v, %v", filepath.Base(root), keys, err)
		}
		if keys, err := ListFolder(root, ExportFolder()); err != nil || !slices.Equal(keys, []string{"Main.txt", "leak.txt"}) {
			t.Errorf("flat ListFolder of the symlinked root %s = %v, %v", filepath.Base(root), keys, err)
		}
	}
	if doc, err := ParseFolderTree(link, TreeFolder()); err != nil {
		t.Errorf("ParseFolderTree of a symlinked root: %v", err)
	} else if len(doc.Files) != 2 || doc.FilePath != link {
		t.Errorf("ParseFolderTree of a symlinked root: %d files, FilePath %q", len(doc.Files), doc.FilePath)
	}

	// A symlinked file named as the root is still not a folder.
	fileLink := filepath.Join(t.TempDir(), "file.txt")
	symlinkOrSkip(t, filepath.Join(target, "Main.txt"), fileLink)
	if keys, err := ListFolder(fileLink, TreeFolder()); err == nil {
		t.Errorf("ListFolder of a symlink to a file = %v, want an error", keys)
	}
}

// A junction (mklink /J, which needs no privilege) is Windows' usual folder
// link: named as the root it is followed like a symlink (the walk said "not a
// directory"), and one inside the tree is not walked.
func TestListFolder_JunctionRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are a Windows feature")
	}
	junction := func(link, target string) {
		t.Helper()
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("mklink /J: %v: %s", err, out)
		}
	}
	outside := writeFolder(t, map[string]string{"Secret.txt": memberFlow})
	target := writeFolder(t, map[string]string{"Main.txt": memberFlow})
	junction(filepath.Join(target, "inner"), outside)
	link := filepath.Join(t.TempDir(), "flows")
	junction(link, target)
	for _, opts := range []FolderOptions{TreeFolder(), ExportFolder()} {
		if keys, err := ListFolder(link, opts); err != nil || !slices.Equal(keys, []string{"Main.txt"}) {
			t.Errorf("ListFolder of a junction root (recursive %t) = %v, %v", opts.Recursive, keys, err)
		}
	}
	if files, err := ReadFolder(link, TreeFolder()); err != nil || len(files) != 1 {
		t.Errorf("ReadFolder of a junction root = %d files, %v", len(files), err)
	}
}

// TestParseFolderTree pins the document bakicli has always built: slash keys
// as SourceFile and Files[].Path (so subflow names, fingerprints, baselines
// and SARIF URIs stay put), a root Main first, the folder as FilePath.
func TestParseFolderTree(t *testing.T) {
	dir := writeFolder(t, map[string]string{
		"Main.txt":          memberFlow,
		"Helper.pad":        memberFlow,
		"teamA/Main.txt":    memberFlow,
		"teamB/Main.txt":    memberFlow,
		"vendor/Legacy.txt": memberFlow,
		".git/Bogus.txt":    memberFlow,
		"notes.md":          "not a flow",
		".bakiignore":       "vendor/\n",
	})
	doc, err := ParseFolderTree(dir, TreeFolder())
	if err != nil {
		t.Fatal(err)
	}
	var sources, names, paths []string
	for _, sf := range doc.Subflows {
		sources = append(sources, sf.SourceFile)
		names = append(names, sf.Name)
	}
	for _, f := range doc.Files {
		paths = append(paths, f.Path)
	}
	wantKeys := []string{"Main.txt", "Helper.pad", "teamA/Main.txt", "teamB/Main.txt"}
	if !slices.Equal(sources, wantKeys) || !slices.Equal(paths, wantKeys) {
		t.Errorf("SourceFile %v, Files[].Path %v, want %v", sources, paths, wantKeys)
	}
	// The implicit subflow is named after the key: part of every fingerprint.
	if want := []string{"Main", "Helper", "teamA/Main", "teamB/Main"}; !slices.Equal(names, want) {
		t.Errorf("subflow names %v, want %v", names, want)
	}
	if doc.FilePath != dir || doc.Name != filepath.Base(dir) || !doc.IsFolder || !strings.HasPrefix(doc.StableID, "files-") {
		t.Errorf("FilePath %q Name %q IsFolder %t StableID %q", doc.FilePath, doc.Name, doc.IsFolder, doc.StableID)
	}

	single, err := ParseFolderTree(writeFolder(t, map[string]string{"Main.pad": memberFlow}), TreeFolder())
	if err != nil {
		t.Fatal(err)
	}
	if single.IsFolder || len(single.Subflows) != 1 || single.Subflows[0].Name != "Main" {
		t.Errorf("a one-member folder: IsFolder %t, subflows %+v", single.IsFolder, single.Subflows)
	}

	empty := writeFolder(t, map[string]string{"notes.md": "x", "vendor/A.txt": memberFlow, ".bakiignore": "vendor/"})
	if _, err := ParseFolderTree(empty, TreeFolder()); err == nil || err.Error() != "no .txt or .pad files found in "+empty {
		t.Errorf("empty folder: err = %v", err)
	}
	if _, err := ParseFolderTree(empty, ExportFolder()); err == nil || err.Error() != "no .txt files found in "+empty {
		t.Errorf("empty export folder: err = %v", err)
	}
}

// TestParseFolder_DesktopShape pins what the desktop folder open depends on,
// now that ParseFolder discovers its members through the shared walker.
func TestParseFolder_DesktopShape(t *testing.T) {
	dir := writeFolder(t, map[string]string{
		"Main.txt":    memberFlow,
		"B.txt":       memberFlow,
		"a.TXT":       memberFlow,
		".hidden.txt": memberFlow,
		"Helper.pad":  memberFlow,
		"sub/X.txt":   memberFlow,
		"dir.txt/Y":   memberFlow,
		".bakiignore": "B.txt\n",
	})
	doc, err := ParseFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names, paths, sources []string
	for _, f := range doc.Files {
		names = append(names, f.Name)
		paths = append(paths, f.Path)
	}
	for _, sf := range doc.Subflows {
		sources = append(sources, sf.SourceFile)
	}
	want := []string{"Main.txt", ".hidden.txt", "B.txt", "a.TXT"}
	if !slices.Equal(names, want) || !slices.Equal(sources, want) {
		t.Errorf("members %v, SourceFile %v, want %v (flat, .txt only, no ignore file)", names, sources, want)
	}
	for i, p := range paths {
		if p != filepath.Join(dir, want[i]) {
			t.Errorf("Files[%d].Path = %q, want the absolute path", i, p)
		}
	}
	if !doc.IsFolder || doc.StableID != "" || doc.FilePath != dir || doc.Name != filepath.Base(dir) {
		t.Errorf("IsFolder %t StableID %q FilePath %q Name %q", doc.IsFolder, doc.StableID, doc.FilePath, doc.Name)
	}

	if _, err := ParseFolder(writeFolder(t, map[string]string{"Main.pad": memberFlow})); err == nil ||
		!strings.HasPrefix(err.Error(), "no .txt files found in ") {
		t.Errorf("a .pad-only folder: err = %v", err)
	}
}

// The desktop follows a symlinked member, as it always has.
func TestParseFolder_FollowsSymlinks(t *testing.T) {
	target := filepath.Join(t.TempDir(), "Other.txt")
	if err := os.WriteFile(target, []byte(memberFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := writeFolder(t, map[string]string{"Main.txt": memberFlow})
	symlinkOrSkip(t, target, filepath.Join(dir, "Linked.txt"))
	doc, err := ParseFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Files) != 2 || doc.Files[1].Name != "Linked.txt" {
		t.Errorf("Files = %+v, want Main.txt and Linked.txt", doc.Files)
	}
}

func TestFolderOptions_Fresh(t *testing.T) {
	TreeFolder().Extensions[0] = ".mutated"
	if TreeFolder().Extensions[0] != ".txt" {
		t.Error("TreeFolder shares its Extensions slice")
	}
}
