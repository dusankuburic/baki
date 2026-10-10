package di

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

	"pad-core/cache"

	"pad-analyzer/internal/platform/eventbus"
)

var updateCaches = flag.Bool("update-caches", false, "rewrite testdata/caches.golden")

// A service fences its fills with its eventbus.Gen; core declares the method set it needs.
var _ cache.FillGuard = (*eventbus.Gen[string])(nil)

// TestCacheRegistry_Golden records every declared cache (ADR 0017): its name, the key parts each key must carry,
// whether it is keyed by a tenant or deliberately global (and why), and where its entries live. This package wires
// the whole app, so every declaring package is linked into its tests (TestCacheRegistry_CoversEveryDeclaration
// checks that). Review a change like a route-policy change; refresh only for an intended one:
// go test ./internal/di/ -run TestCacheRegistry_Golden -update-caches
func TestCacheRegistry_Golden(t *testing.T) {
	var b strings.Builder
	for _, s := range cache.Specs() {
		kinds := make([]string, len(s.Kinds))
		for i, k := range s.Kinds {
			kinds[i] = k.String()
		}
		keys := strings.Join(kinds, ", ")
		if keys == "" {
			keys = "none"
		}
		scope := "tenant"
		if s.Global != "" {
			scope = "global: " + s.Global
		}
		fmt.Fprintf(&b, "%s | keys: %s | %s | %s: %s\n", s.Name, keys, scope, s.Locality, s.Where)
	}
	got := b.String()
	path := filepath.Join("testdata", "caches.golden")
	if *updateCaches {
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
		t.Fatalf("read golden: %v (create it with -update-caches)", err)
	}
	if want := strings.ReplaceAll(string(raw), "\r\n", "\n"); got != want {
		t.Errorf("declared caches changed; review, then refresh with -update-caches.\n--- golden\n%s--- now\n%s", want, got)
	}
}

// TestCacheRegistry_CoversEveryDeclaration: every cache.Declare in the source of both modules (the root package,
// internal/, cmd/ and core/) is registered in this test binary, so the golden lists it. A declaration in a package the app graph does not link (or in package main)
// would be missing from the golden; move it, or import its package here.
func TestCacheRegistry_CoversEveryDeclaration(t *testing.T) {
	registered := map[string]bool{}
	for _, s := range cache.Specs() {
		registered[s.Name] = true
	}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	check := func(p string) error {
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, name := range declaredCacheNames(f) {
			if !registered[name] {
				rel, _ := filepath.Rel(root, p)
				t.Errorf("%s declares cache %q, which this test binary does not register: the golden would miss it",
					filepath.ToSlash(rel), name)
			}
		}
		return nil
	}
	// The root package (main.go) first: package main is never linked into a test binary of another package.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			if err := check(filepath.Join(root, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, dir := range []string{"internal", "cmd", "core"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if d.IsDir() {
				return nil
			}
			return check(p)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// declaredCacheNames returns the literal names f passes to pad-core/cache's Declare.
func declaredCacheNames(f *ast.File) []string {
	pkg := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "pad-core/cache" {
			pkg = "cache"
			if imp.Name != nil {
				pkg = imp.Name.Name
			}
		}
	}
	if pkg == "" {
		return nil
	}
	var names []string
	ast.Inspect(f, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Declare" {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != pkg {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			name, _ := strconv.Unquote(lit.Value)
			names = append(names, name)
		}
		return true
	})
	return names
}
