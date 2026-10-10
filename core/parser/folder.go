package parser

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"pad-core/models"
)

// FolderOptions selects which files under a folder make up one flow.
type FolderOptions struct {
	// Recursive walks subdirectories; keys are then slash paths relative to
	// the folder. Otherwise only the folder's own files are members.
	Recursive bool
	// Extensions are the member suffixes, lower-case with the dot, matched
	// case-insensitively. Empty means .txt and .pad.
	Extensions []string
	// IgnoreFile names an ignore file in the folder (".bakiignore"); see
	// IgnoreList. Empty reads none.
	IgnoreFile string
	// SkipDotDirs never descends into a directory whose name starts with a dot
	// (.git, .github).
	SkipDotDirs bool
	// Confined skips symlinks and reads through an os.Root, so nothing outside
	// the folder is read. For untrusted trees: a parse error's snippet is
	// verbatim source, and CI uploads it.
	Confined bool
	// MaxFileSize rejects a larger member with *FileTooLargeError before it is
	// read; ListFolder checks it on the listing, before anything is read. Zero
	// leaves the limit to the parser (MaxParseTextSize, checked after reading).
	MaxFileSize int64
}

// TreeFolder is the file set bakicli analyzes: every .txt and .pad file under
// the folder, outside dot directories and .bakiignore matches, symlinks
// skipped.
func TreeFolder() FolderOptions {
	return FolderOptions{
		Recursive:   true,
		Extensions:  []string{".txt", ".pad"},
		IgnoreFile:  ".bakiignore",
		SkipDotDirs: true,
		Confined:    true,
	}
}

// ExportFolder is a PAD flow-export folder as the desktop opens it: the .txt
// files directly inside it, symlinks followed.
func ExportFolder() FolderOptions {
	return FolderOptions{Extensions: []string{".txt"}}
}

// FileTooLargeError reports a member over FolderOptions.MaxFileSize.
type FileTooLargeError struct {
	Name      string // the member's key
	Size, Max int64  // bytes
}

func (e *FileTooLargeError) Error() string {
	return fmt.Sprintf("%s is %d bytes (limit %d bytes)", e.Name, e.Size, e.Max)
}

// ListFolder returns the keys of dir's members, sorted, and reads nothing. A
// member over MaxFileSize (its stat size; a followed symlink's target) fails
// the listing with *FileTooLargeError.
func ListFolder(dir string, opts FolderOptions) ([]string, error) {
	var keys []string
	err := walkFolder(dir, opts, func(key string, d fs.DirEntry) error {
		if opts.MaxFileSize > 0 {
			if size, ok := entrySize(dir, key, d); ok && size > opts.MaxFileSize {
				return &FileTooLargeError{Name: key, Size: size, Max: opts.MaxFileSize}
			}
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read folder: %w", err)
	}
	sort.Strings(keys)
	return keys, nil
}

// ReadFolder reads dir's members into key -> text. The first error aborts.
func ReadFolder(dir string, opts FolderOptions) (map[string]string, error) {
	open := func(key string) (*os.File, error) {
		return os.Open(filepath.Join(dir, filepath.FromSlash(key))) // #nosec G304 -- a member of the folder the caller chose
	}
	if opts.Confined {
		root, err := os.OpenRoot(dir)
		if err != nil {
			return nil, fmt.Errorf("open folder: %w", err)
		}
		defer func() { _ = root.Close() }()
		open = root.Open
	}
	files := make(map[string]string)
	err := walkFolder(dir, opts, func(key string, _ fs.DirEntry) error {
		text, err := readMember(open, key, opts.MaxFileSize)
		if err != nil {
			return err
		}
		files[key] = text
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read folder: %w", err)
	}
	return files, nil
}

// ParseFolderTree parses dir's members as one flow named after the folder
// (ParseFiles keys: Main first, then by key), with FilePath set to dir.
func ParseFolderTree(dir string, opts FolderOptions) (*models.FlowDocument, error) {
	files, err := ReadFolder(dir, opts)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no %s files found in %s", strings.Join(opts.extensions(), " or "), dir)
	}
	doc, err := ParseFiles(files, filepath.Base(dir))
	if err != nil {
		return nil, err
	}
	doc.FilePath = dir
	return doc, nil
}

// IgnoreList holds the patterns of an ignore file: one per line, blank lines
// and `#` comments skipped. A pattern matches a member's slash path relative
// to the folder, its base name, or, ending in "/", a directory prefix; a
// matching directory is not walked. Matching is path.Match on every OS: `*`
// never crosses "/", and a backslash escapes the next character.
type IgnoreList []string

// ParseIgnoreList parses ignore-file text (LF or CRLF, an optional UTF-8 BOM).
func ParseIgnoreList(text string) IgnoreList {
	text = strings.TrimPrefix(text, "\ufeff")
	var l IgnoreList
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		l = append(l, line)
	}
	return l
}

// ReadIgnoreFile reads dir/name; a missing or unreadable file ignores nothing.
func ReadIgnoreFile(dir, name string) IgnoreList {
	data, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- the ignore file of a folder the caller chose
	if err != nil {
		return nil
	}
	return ParseIgnoreList(string(data))
}

// Match reports whether the slash path rel (relative to the folder) is ignored.
func (l IgnoreList) Match(rel string) bool {
	for _, p := range l {
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(rel)); ok {
			return true
		}
		if strings.HasSuffix(p, "/") && strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

// walkFolder calls visit with the key and entry of every member of dir, in
// walk order (lexical within each directory). A recursive walk never follows
// a symlinked directory; a flat one lists dir with os.ReadDir.
func walkFolder(dir string, opts FolderOptions, visit func(key string, d fs.DirEntry) error) error {
	var ignore IgnoreList
	if opts.IgnoreFile != "" {
		ignore = ReadIgnoreFile(dir, opts.IgnoreFile)
	}
	if !opts.Recursive {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, d := range entries {
			if d.IsDir() || !opts.isMember(d, d.Name(), ignore) {
				continue
			}
			if err := visit(d.Name(), d); err != nil {
				return err
			}
		}
		return nil
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			// A symlinked root is not walked (it lists nothing), as before.
			if !d.IsDir() && d.Type()&fs.ModeSymlink == 0 {
				return fmt.Errorf("%s: not a directory", p)
			}
			return nil
		}
		if d.IsDir() {
			if opts.SkipDotDirs && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			// Pruned, so "vendor/" costs nothing on a large tree.
			if ignore.Match(rel+"/") || ignore.Match(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if !opts.isMember(d, rel, ignore) {
			return nil
		}
		return visit(rel, d)
	})
}

// isMember filters a non-directory entry. ReadDir and WalkDir report a
// symlink as itself (Lstat), never as its target.
func (o FolderOptions) isMember(d fs.DirEntry, key string, ignore IgnoreList) bool {
	name := strings.ToLower(d.Name())
	ok := false
	for _, ext := range o.extensions() {
		if strings.HasSuffix(name, ext) {
			ok = true
			break
		}
	}
	if !ok || ignore.Match(key) {
		return false
	}
	return !o.Confined || d.Type()&fs.ModeSymlink == 0
}

func (o FolderOptions) extensions() []string {
	if len(o.Extensions) == 0 {
		return []string{".txt", ".pad"}
	}
	return o.Extensions
}

// entrySize is a member's size without opening it; a symlink member (only an
// unconfined walk has one) reports its target's. ok is false when stat fails:
// the read reports that.
func entrySize(dir, key string, d fs.DirEntry) (size int64, ok bool) {
	var info fs.FileInfo
	var err error
	if d.Type()&fs.ModeSymlink != 0 {
		info, err = os.Stat(filepath.Join(dir, filepath.FromSlash(key)))
	} else {
		info, err = d.Info()
	}
	if err != nil {
		return 0, false
	}
	return info.Size(), true
}

// readMember reads one member through open, rejecting it before the read when
// it is over max (> 0).
func readMember(open func(string) (*os.File, error), key string, max int64) (string, error) {
	f, err := open(key)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", key, err)
	}
	defer func() { _ = f.Close() }()
	var size int64
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	if max > 0 && size > max {
		return "", &FileTooLargeError{Name: key, Size: size, Max: max}
	}
	var r io.Reader = f
	if max > 0 {
		r = io.LimitReader(f, max+1)
	}
	var sb strings.Builder
	if size > 0 && size <= MaxParseTextSize {
		sb.Grow(int(size))
	}
	if _, err := io.Copy(&sb, r); err != nil {
		return "", fmt.Errorf("read %s: %w", key, err)
	}
	if max > 0 && int64(sb.Len()) > max {
		return "", &FileTooLargeError{Name: key, Size: int64(sb.Len()), Max: max}
	}
	return sb.String(), nil
}
