package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Caches are declared (ADR 0017): a package-level cache.Declare names the cache and the key parts it requires, and
// cache.New builds its storage. These ratchets pin the caches built the old way, which wave 2 and 3 of Phase 4a
// move onto declared specs, and keep the declarations themselves statically visible. core/cache, which implements
// the building blocks, is not counted.

// coreSourceFiles parses every non-test file of the pad-core module (core/), keyed by its slash path from the repo
// root like sourceFiles.
func coreSourceFiles(t *testing.T) []goFile {
	t.Helper()
	root := repoRoot(t)
	fset := token.NewFileSet()
	var out []goFile
	err := filepath.WalkDir(filepath.Join(root, "core"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, goFile{path: filepath.ToSlash(rel), ast: f})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// cacheScanFiles is every non-test file of both modules except core/cache's.
func cacheScanFiles(t *testing.T) []goFile {
	var out []goFile
	for _, f := range append(sourceFiles(t), coreSourceFiles(t)...) {
		if !strings.HasPrefix(f.path, "core/cache/") {
			out = append(out, f)
		}
	}
	return out
}

var lruMajorVersion = regexp.MustCompile(`^v[0-9]+$`)

// importNames maps the name f refers to an import by (its alias, else the package's default name) to the import
// path, for the imports match selects.
func importNames(f *ast.File, match func(path string) (defaultName string, ok bool)) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name, ok := match(p)
		if !ok {
			continue
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = p
	}
	return out
}

// countSelectors counts the references pkg.Name (called or not) where pkg is one of names and want accepts the
// import path and Name.
func countSelectors(f *ast.File, names map[string]string, want func(path, sel string) bool) int {
	if len(names) == 0 {
		return 0
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok {
			if p, imported := names[x.Name]; imported && want(p, sel.Sel.Name) {
				n++
			}
		}
		return true
	})
	return n
}

// TestArch_RawCacheConstructionsRatchet: a cache built with cache.NewLRUCache, cache.NewSizedLRU or a golang-lru
// constructor has no declared tenant, no statistics and no entry in the caches golden. New caches declare a spec
// and use cache.New; the ones left are pinned per file and only go down.
func TestArch_RawCacheConstructionsRatchet(t *testing.T) {
	got := map[string]int{}
	for _, f := range cacheScanFiles(t) {
		names := importNames(f.ast, func(p string) (string, bool) {
			switch {
			case p == "pad-core/cache":
				return "cache", true
			case p == "github.com/hashicorp/golang-lru" || strings.HasPrefix(p, "github.com/hashicorp/golang-lru/"):
				name := p[strings.LastIndex(p, "/")+1:]
				if name == "golang-lru" || lruMajorVersion.MatchString(name) {
					name = "lru"
				}
				return name, true
			}
			return "", false
		})
		n := countSelectors(f.ast, names, func(p, sel string) bool {
			if p == "pad-core/cache" {
				return sel == "NewLRUCache" || sel == "NewSizedLRU"
			}
			return strings.HasPrefix(sel, "New")
		})
		if n > 0 {
			got[f.path] = n
		}
	}
	checkRatchet(t, "raw cache constructions (declare a spec and use cache.New)", got, rawCacheConstructions)
}

// rawCacheConstructions is the number of raw cache constructions per file.
var rawCacheConstructions = map[string]int{
	"internal/service/flow_snapshots.go": 1, // undo rings: per-flow state, not a memo; stays
}

// TestArch_SingleflightGroupsRatchet: a hand-rolled flight is only as safe as its key; one coarser than the cache key
// shares a result between callers the cache keeps apart (one tenant's fill served to another). cache.Typed's
// GetOrFill flies on the full key. The groups left are pinned per file and only go down.
func TestArch_SingleflightGroupsRatchet(t *testing.T) {
	got := map[string]int{}
	for _, f := range cacheScanFiles(t) {
		names := importNames(f.ast, func(p string) (string, bool) {
			return "singleflight", p == "golang.org/x/sync/singleflight"
		})
		if n := countSelectors(f.ast, names, func(_, sel string) bool { return sel == "Group" }); n > 0 {
			got[f.path] = n
		}
	}
	checkRatchet(t, "singleflight.Group declarations (use cache.Typed.GetOrFill)", got, singleflightGroups)
}

// singleflightGroups is the number of singleflight.Group references (fields and variables) per file. The flights
// left in internal/service fly on their cache's full key; each serves an entry only after a check GetOrFill
// cannot make (the stored version, the exact document object, an org's or the user's fences).
var singleflightGroups = map[string]int{
	"core/analyzer/cache.go":                1, // analysis walks, on the full report key (the report store is not a Typed)
	"internal/ai/azure_entra.go":            1, // Entra token source
	"internal/ai/azure_foundry.go":          1, // Foundry model listings
	"internal/ai/copilot_auth.go":           1, // Copilot session tokens
	"internal/ai/model_list_cache.go":       1, // provider model listings
	"internal/service/chat_turn_memo.go":    1, // per-turn memo
	"internal/service/dashboard.go":         1, // analytics (entries dropped by the orgs they span)
	"internal/service/document_provider.go": 1, // cloud documents (served at the stored version only)
	"internal/service/flow.go":              1, // search indexes (served for the exact document only)
	"internal/service/rule_profile.go":      1, // rule profiles (an org's invalidation drops every reader's)
	"internal/storage/database/azure.go":    1, // Azure registry reads
}

// TestArch_CacheDeclarationsArePackageLevel: cache.Declare runs once per kind, as the value of a package-level var,
// with a literal name unique across both modules. So every declaration exists as soon as its package is imported
// (the caches golden sees it, a duplicate fails at init), and the metrics' cache label is a closed set.
func TestArch_CacheDeclarationsArePackageLevel(t *testing.T) {
	declared := map[string]string{} // name -> file
	for _, f := range cacheScanFiles(t) {
		names := importNames(f.ast, func(p string) (string, bool) { return "cache", p == "pad-core/cache" })
		if len(names) == 0 {
			continue
		}
		isDeclare := func(e ast.Expr) bool {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			x, ok := sel.X.(*ast.Ident)
			return ok && names[x.Name] != "" && sel.Sel.Name == "Declare"
		}
		allowed := map[ast.Expr]bool{}
		for _, decl := range f.ast.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, v := range spec.(*ast.ValueSpec).Values {
					call, ok := v.(*ast.CallExpr)
					if !ok || !isDeclare(call.Fun) {
						continue
					}
					allowed[call.Fun] = true
					lit, ok := call.Args[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Errorf("%s: cache.Declare needs a literal name", f.path)
						continue
					}
					name, _ := strconv.Unquote(lit.Value)
					if prev, dup := declared[name]; dup {
						t.Errorf("cache %q is declared in %s and %s", name, prev, f.path)
					}
					declared[name] = f.path
				}
			}
		}
		ast.Inspect(f.ast, func(node ast.Node) bool {
			if e, ok := node.(ast.Expr); ok && isDeclare(e) && !allowed[e] {
				t.Errorf("%s: cache.Declare outside a package-level var (declare each cache once, at package level)", f.path)
			}
			return true
		})
	}
}
