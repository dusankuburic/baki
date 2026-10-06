// Package archtest holds the layering and convention ratchets of the backend (Phase 0a of
// docs/architecture/backend-refactoring-proposal.md). Each rule pins today's exceptions: new code may not add to
// them, and when an exception goes away the test asks for its number to be lowered so it cannot creep back.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// goFile is one parsed non-test Go file of the root module, keyed by its slash path from the repo root.
type goFile struct {
	path string
	ast  *ast.File
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repo root")
	}
	return filepath.Join(filepath.Dir(here), "..", "..")
}

// sourceFiles parses main.go and every non-test file under internal/ and cmd/.
func sourceFiles(t *testing.T) []goFile {
	t.Helper()
	root := repoRoot(t)
	fset := token.NewFileSet()
	var out []goFile
	parse := func(abs string) {
		f, err := parser.ParseFile(fset, abs, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", abs, err)
		}
		rel, _ := filepath.Rel(root, abs)
		out = append(out, goFile{path: filepath.ToSlash(rel), ast: f})
	}
	parse(filepath.Join(root, "main.go"))
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
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

func imports(f *ast.File, path string) bool {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == path || strings.HasPrefix(p, path+"/") {
			return true
		}
	}
	return false
}

// countCalls counts calls whose function matches.
func countCalls(f *ast.File, match func(fun ast.Expr) bool) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && match(call.Fun) {
			n++
		}
		return true
	})
	return n
}

// methodCall matches x.<name>(...) for any receiver.
func methodCall(names ...string) func(ast.Expr) bool {
	return func(fun ast.Expr) bool {
		sel, ok := fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		return slices.Contains(names, sel.Sel.Name)
	}
}

// checkRatchet compares per-file counts with the pinned allowance: more than pinned (or a new file) fails, and
// so does less, so the allowance is lowered and cannot creep back up.
func checkRatchet(t *testing.T, what string, got, allowed map[string]int) {
	t.Helper()
	files := map[string]bool{}
	for f := range got {
		files[f] = true
	}
	for f := range allowed {
		files[f] = true
	}
	var names []string
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		switch g, a := got[f], allowed[f]; {
		case g > a:
			t.Errorf("%s: %s has %d, allowed %d — move this out instead of adding to it", what, f, g, a)
		case g < a:
			t.Errorf("%s: %s is down to %d (allowed %d) — lower its allowance in archtest to %d", what, f, g, a, g)
		}
	}
}

// TestArch_APIDoesNotImportStorageDatabase: handlers reach storage through services and interfaces, never the
// Postgres package (post-commit work goes through internal/platform/txhook). One exception is left.
func TestArch_APIDoesNotImportStorageDatabase(t *testing.T) {
	got := map[string]int{}
	for _, f := range sourceFiles(t) {
		if strings.HasPrefix(f.path, "internal/api/") && imports(f.ast, "pad-analyzer/internal/storage/database") {
			got[f.path] = 1
		}
	}
	checkRatchet(t, "internal/api importing storage/database", got, map[string]int{
		"internal/api/rls_middleware.go": 1, // the RLS scope and its hook runner; Phase 3 injects them instead
	})
}

// TestArch_ServiceDoesNotImportAPI: the service layer must stay usable without HTTP (jobs, CLI, workers).
func TestArch_ServiceDoesNotImportAPI(t *testing.T) {
	for _, f := range sourceFiles(t) {
		if strings.HasPrefix(f.path, "internal/service/") && imports(f.ast, "pad-analyzer/internal/api") {
			t.Errorf("%s imports internal/api", f.path)
		}
	}
}

// TestArch_TransactionsOpenedOnlyWhereAllowed: a transaction opened beside the request's RLS transaction takes a
// second pooled connection and runs without app.current_user_id (RLS allow-all). New writes go through withTx.
func TestArch_TransactionsOpenedOnlyWhereAllowed(t *testing.T) {
	got := map[string]int{}
	for _, f := range sourceFiles(t) {
		if n := countCalls(f.ast, methodCall("BeginTx", "BeginRLS")); n > 0 {
			got[f.path] = n
		}
	}
	checkRatchet(t, "BeginTx/BeginRLS calls", got, map[string]int{
		"internal/storage/database/rls.go":                 3, // withTx fallback, BeginRLS itself, the lazy scope
		"internal/storage/database/postgres_migrations.go": 2, // one transaction per step, up and down
		"internal/storage/database/postgres_users.go":      2, // serializable CreateUser, audit batch
		"internal/storage/database/postgres_orgs.go":       4, // self-managed RLS transactions
		"internal/storage/database/postgres_triage.go":     1,
	})
}

// TestArch_HandlerStorageCallsRatchet: handlers calling the storage backend directly bypass the service layer
// (authorization, audit, cache invalidation). Phase 2a moves them into services; the count only goes down.
func TestArch_HandlerStorageCallsRatchet(t *testing.T) {
	backendCall := func(fun ast.Expr) bool {
		sel, ok := fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		switch x := sel.X.(type) {
		case *ast.SelectorExpr:
			return x.Sel.Name == "backend"
		case *ast.Ident:
			return x.Name == "backend"
		}
		return false
	}
	total := 0
	for _, f := range sourceFiles(t) {
		if strings.HasPrefix(f.path, "internal/api/") {
			total += countCalls(f.ast, backendCall)
		}
	}
	checkRatchet(t, "backend.X(...) calls in internal/api", map[string]int{"internal/api": total},
		map[string]int{"internal/api": handlerBackendCallAllowance})
}

// handlerBackendCallAllowance is the number of direct storage calls left in internal/api.
const handlerBackendCallAllowance = 85

// TestArch_EnvReadsOnlyInConfig: configuration is read in internal/config, so it is validated once and visible in
// one place. Exempt: the CLI, test tooling, and the standard OTEL_* variables read by telemetry.
func TestArch_EnvReadsOnlyInConfig(t *testing.T) {
	exempt := []string{"internal/config/", "cmd/", "internal/testutil/", "internal/storage/contract/"}
	got := map[string]int{}
	for _, f := range sourceFiles(t) {
		skip := false
		for _, p := range exempt {
			if strings.HasPrefix(f.path, p) {
				skip = true
			}
		}
		if skip {
			continue
		}
		n := 0
		ast.Inspect(f.ast, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv") {
				if len(call.Args) == 1 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && strings.HasPrefix(lit.Value, `"OTEL_`) {
						return true
					}
				}
				n++
			}
			return true
		})
		if n > 0 {
			got[f.path] = n
		}
	}
	checkRatchet(t, "os.Getenv outside internal/config", got, map[string]int{
		"main.go":                               6, // config bootstrap (PAD_CONFIG/PAD_MODE/PAD_PORT), webhook notifier, healthcheck
		"internal/ai/httpclient.go":             1,
		"internal/ai/providerconfig.go":         1,
		"internal/api/handlers_integrations.go": 1,
		"internal/telemetry/telemetry.go":       1, // PAD_MODE as the deployment environment
	})
}
