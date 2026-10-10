package lifecycle_test

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"pad-analyzer/internal/lifecycle"

	// Every package that declares a job, so the registry below is complete (TestJobRegistry_CoversEveryDeclaration).
	_ "pad-analyzer/internal/api"
	_ "pad-analyzer/internal/app"
	_ "pad-analyzer/internal/audit"
	_ "pad-analyzer/internal/scanner"
	_ "pad-analyzer/internal/storage/database"
)

var updateJobs = flag.Bool("update-jobs", false, "rewrite testdata/jobs.golden")

// repoRoot is the module root, two levels up.
var repoRoot = filepath.Join("..", "..")

// sourceFiles parses every non-test Go file of the module: the root package, internal/ and cmd/.
func sourceFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	out := map[string]*ast.File{}
	fset := token.NewFileSet()
	parse := func(p string) {
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(repoRoot, p)
		out[filepath.ToSlash(rel)] = f
	}
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			parse(filepath.Join(repoRoot, e.Name()))
		}
	}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if !d.IsDir() {
				parse(p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// declaredJobs returns the job names declared in the source, by file: the Name of each DeclareJob(JobDecl{...}).
func declaredJobs(t *testing.T, files map[string]*ast.File) map[string]string {
	t.Helper()
	out := map[string]string{}
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			var fn string
			switch x := call.Fun.(type) {
			case *ast.SelectorExpr:
				fn = x.Sel.Name
			case *ast.Ident:
				fn = x.Name
			}
			lit, ok := call.Args[0].(*ast.CompositeLit)
			if fn != "DeclareJob" || !ok {
				return true
			}
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Name" {
					v, ok := kv.Value.(*ast.BasicLit)
					if !ok || v.Kind != token.STRING {
						t.Errorf("%s: DeclareJob's Name must be a string literal", path)
						continue
					}
					name, _ := strconv.Unquote(v.Value)
					out[name] = path
				}
			}
			return true
		})
	}
	return out
}

// lockKeyConst is a constant named *LockKey in the source: every advisory-lock key of the code base is one (the
// jobs', the transaction locks', the schema migration's).
type lockKeyConst struct {
	file, name string
	value      int64
}

func lockKeyConsts(t *testing.T, files map[string]*ast.File) []lockKeyConst {
	t.Helper()
	var out []lockKeyConst
	for path, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if !strings.HasSuffix(id.Name, "LockKey") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.INT {
						t.Errorf("%s: %s must be an integer literal, so the key tests can read it", path, id.Name)
						continue
					}
					v, err := strconv.ParseInt(lit.Value, 0, 64)
					if err != nil {
						t.Errorf("%s: %s = %s: %v", path, id.Name, lit.Value, err)
						continue
					}
					out = append(out, lockKeyConst{path, id.Name, v})
				}
			}
		}
	}
	return out
}

// keyText renders a key as hex, with its ASCII spelling when it has one ("bakiscan").
func keyText(key int64) string {
	b := make([]byte, 8)
	for i := range 8 {
		b[i] = byte(uint64(key) >> (56 - 8*i))
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("%#016x", uint64(key))
		}
	}
	return fmt.Sprintf("%#016x %q", uint64(key), string(b))
}

// TestJobRegistry_Golden lists every job the source declares: its name, lock key (legacy or derived) and schedule.
// Review a change like a route-policy change: a changed legacy key lets two revisions run the job side by side
// during a rolling deploy. Refresh only for an intended change:
// go test ./internal/lifecycle/ -run TestJobRegistry_Golden -update-jobs
func TestJobRegistry_Golden(t *testing.T) {
	declared := declaredJobs(t, sourceFiles(t))
	var b strings.Builder
	for _, j := range lifecycle.Jobs() {
		if _, inSource := declared[j.Name]; !inSource {
			continue // a test's job
		}
		origin := "derived"
		if j.Legacy {
			origin = "legacy"
		}
		fmt.Fprintf(&b, "%s | key %s (%s) | %s\n", j.Name, keyText(j.Key), origin, j.Schedule)
	}
	got := b.String()
	path := filepath.Join("testdata", "jobs.golden")
	if *updateJobs {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (create it with -update-jobs)", err)
	}
	if want := strings.ReplaceAll(string(raw), "\r\n", "\n"); got != want {
		t.Errorf("declared jobs changed; review, then refresh with -update-jobs.\n--- golden\n%s--- now\n%s", want, got)
	}
}

// TestJobRegistry_CoversEveryDeclaration: every DeclareJob in the source is registered in this test binary, so the
// golden lists it. A declaration in a package not imported above (or in package main) would be missing; import it.
func TestJobRegistry_CoversEveryDeclaration(t *testing.T) {
	registered := map[string]bool{}
	for _, j := range lifecycle.Jobs() {
		registered[j.Name] = true
	}
	declared := declaredJobs(t, sourceFiles(t))
	if len(declared) < 5 {
		t.Fatalf("found %d job declarations in the source; the scan is broken", len(declared))
	}
	for name, path := range declared {
		if !registered[name] {
			t.Errorf("%s declares job %q, which this test binary does not register: import its package here", path, name)
		}
	}
}

// TestJobKeys_Unique: the advisory-lock keys share one int8 key space. Every *LockKey constant of the source (read
// from the source, so a changed constant is seen, not a hand copy of it) differs from every other, a job keeping a
// legacy key keeps one of them, and a derived key is its name's hash and differs from all of them.
func TestJobKeys_Unique(t *testing.T) {
	files := sourceFiles(t)
	consts := lockKeyConsts(t, files)
	if len(consts) < 10 {
		t.Fatalf("found %d *LockKey constants; the scan is broken (there were 10 when it was written)", len(consts))
	}
	byValue := map[int64]lockKeyConst{}
	for _, c := range consts {
		if other, dup := byValue[c.value]; dup {
			t.Errorf("%s.%s and %s.%s are the same advisory key %s", c.file, c.name, other.file, other.name, keyText(c.value))
		}
		byValue[c.value] = c
	}
	declared := declaredJobs(t, files)
	seen := map[int64]string{}
	for _, j := range lifecycle.Jobs() {
		if _, inSource := declared[j.Name]; !inSource {
			continue
		}
		if other, dup := seen[j.Key]; dup {
			t.Errorf("jobs %s and %s share the key %s", j.Name, other, keyText(j.Key))
		}
		seen[j.Key] = j.Name
		c, isConst := byValue[j.Key]
		switch {
		case j.Legacy && !isConst:
			t.Errorf("job %s keeps the legacy key %s, which is no *LockKey constant of the source", j.Name, keyText(j.Key))
		case !j.Legacy && j.Key != lifecycle.DerivedJobKey(j.Name):
			t.Errorf("job %s has key %s, not its derived key", j.Name, keyText(j.Key))
		case !j.Legacy && isConst:
			t.Errorf("job %s's derived key %s is %s.%s", j.Name, keyText(j.Key), c.file, c.name)
		}
	}
}
