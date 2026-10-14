package di

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/redisx"
)

var updateRedisKeys = flag.Bool("update-redis-keys", false, "rewrite testdata/redis_keys.golden")

// TestRedisKeys_Golden records every Redis key and channel family the app uses (ADR 0029): name, data structure,
// pattern, TTL, the payload version its owner writes, what the owner does while Redis is down, and the owning
// package. Rolling deploys run two releases against the same keys, so a pattern is never renamed, and every key
// carries a TTL so the server can evict under volatile-lru. This package wires the whole app, so every declaring
// package is linked into its tests (TestRedisKeys_CoversEveryDeclaration checks that). Review a change like a
// route-policy change; refresh only for an intended one:
// go test ./internal/di/ -run TestRedisKeys_Golden -update-redis-keys
func TestRedisKeys_Golden(t *testing.T) {
	var b strings.Builder
	for _, s := range redisx.Specs() {
		b.WriteString(redisx.Describe(s))
		b.WriteByte('\n')
	}
	got := b.String()
	path := filepath.Join("testdata", "redis_keys.golden")
	if *updateRedisKeys {
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
		t.Fatalf("read golden: %v (create it with -update-redis-keys)", err)
	}
	if want := strings.ReplaceAll(string(raw), "\r\n", "\n"); got != want {
		t.Errorf("declared Redis keys changed; review, then refresh with -update-redis-keys.\n--- golden\n%s--- now\n%s", want, got)
	}
}

// TestRedisKeys_CoversEveryDeclaration: every redisx.Declare in the source of the root module is registered in this
// test binary, so the golden lists it. A declaration in a package the app graph does not link would be missing from
// the golden: move it, or import its package here.
func TestRedisKeys_CoversEveryDeclaration(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	calls := 0
	var where []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
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
				return err
			}
			inRedisx := f.Name.Name == "redisx"
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if x, ok := fun.X.(*ast.Ident); ok && x.Name == "redisx" && fun.Sel.Name == "Declare" {
						calls++
						rel, _ := filepath.Rel(root, p)
						where = append(where, filepath.ToSlash(rel))
					}
				case *ast.Ident:
					if inRedisx && fun.Name == "Declare" {
						calls++
						rel, _ := filepath.Rel(root, p)
						where = append(where, filepath.ToSlash(rel))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(redisx.Specs()); got != calls {
		t.Errorf("the source declares %d Redis key families (in %v) but this test binary registered %d: a declaring package is not linked here, so the golden would miss it", calls, where, got)
	}
}

// internal/auth stays free of the Redis dependency, so it keeps its channel name as a constant of its own; the
// declaration lives in redisx. The two must be the same string.
func TestRedisKeys_AuthConstantMatches(t *testing.T) {
	if got := redisx.RevocationChannel.Spec().Pattern; got != auth.RevocationChannel {
		t.Errorf("redisx declares the revocation channel %q, auth.RevocationChannel is %q", got, auth.RevocationChannel)
	}
}

// Every family is honest about its TTL: a declared key has one, a channel has none, which Declare enforces; this
// pins what the deployment relies on (the server runs volatile-lru: a key without a TTL is never evicted).
func TestRedisKeys_EveryStoredKeyHasATTL(t *testing.T) {
	for _, s := range redisx.Specs() {
		if s.Kind != redisx.KindPubSub && s.TTL <= 0 {
			t.Errorf("%s (%s) has no TTL", s.Name, s.Kind)
		}
	}
}
