// Package archtest holds the layering and convention ratchets of the backend (Phase 0a of
// docs/architecture/backend-refactoring-proposal.md). Each rule pins today's exceptions: new code may not add to
// them, and when an exception goes away the test asks for its number to be lowered so it cannot creep back.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
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

// sourceFiles parses every non-test file of the root package (main.go) and under internal/ and cmd/.
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
	rootFiles, err := filepath.Glob(filepath.Join(root, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rootFiles {
		if !strings.HasSuffix(p, "_test.go") {
			parse(p)
		}
	}
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
// Postgres package (post-commit work goes through internal/platform/txhook, the request transaction through
// internal/platform/reqtx, which di provides).
func TestArch_APIDoesNotImportStorageDatabase(t *testing.T) {
	got := map[string]int{}
	for _, f := range sourceFiles(t) {
		if strings.HasPrefix(f.path, "internal/api/") && imports(f.ast, "pad-analyzer/internal/storage/database") {
			got[f.path] = 1
		}
	}
	checkRatchet(t, "internal/api importing storage/database", got, map[string]int{})
}

// TestArch_ServiceDoesNotImportAPI: the service layer must stay usable without HTTP (jobs, CLI, workers).
func TestArch_ServiceDoesNotImportAPI(t *testing.T) {
	for _, f := range sourceFiles(t) {
		if strings.HasPrefix(f.path, "internal/service/") && imports(f.ast, "pad-analyzer/internal/api") {
			t.Errorf("%s imports internal/api", f.path)
		}
	}
}

// TestArch_AuditDoesNotImportAPI: the audit trail records for handlers, services and background jobs alike, so it
// must not depend on the HTTP layer (it reads the request through internal/platform/reqinfo).
func TestArch_AuditDoesNotImportAPI(t *testing.T) {
	for _, f := range sourceFiles(t) {
		if strings.HasPrefix(f.path, "internal/audit/") && imports(f.ast, "pad-analyzer/internal/api") {
			t.Errorf("%s imports internal/api", f.path)
		}
	}
}

// TestArch_EventsStayLeaves: the domain event types import nothing of the module, and the bus no domain package,
// so a publisher and its subscribers depend only on internal/events (ADR 0009).
func TestArch_EventsStayLeaves(t *testing.T) {
	busAllowed := []string{"pad-analyzer/internal/auth", "pad-analyzer/internal/metrics", "pad-analyzer/internal/platform"}
	for _, f := range sourceFiles(t) {
		switch {
		case strings.HasPrefix(f.path, "internal/events/"):
			for _, imp := range f.ast.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); strings.HasPrefix(p, "pad-analyzer/") || strings.HasPrefix(p, "pad-core/") {
					t.Errorf("%s imports %s: event types import nothing of the module", f.path, p)
				}
			}
		case strings.HasPrefix(f.path, "internal/platform/eventbus/"):
			for _, imp := range f.ast.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if !strings.HasPrefix(p, "pad-analyzer/") {
					continue
				}
				ok := false
				for _, allowed := range busAllowed {
					ok = ok || p == allowed || strings.HasPrefix(p, allowed+"/")
				}
				if !ok {
					t.Errorf("%s imports %s: the bus depends on no domain package", f.path, p)
				}
			}
		}
	}
}

// TestArch_TransactionsOpenedOnlyWhereAllowed: a transaction opened beside the request's RLS transaction takes a
// second pooled connection and runs without app.current_user_id (RLS allow-all). New writes go through withTx or
// its variants (withTxAs, withOwnTx), which open their own transactions through openTx; openTx is counted with
// BeginTx and BeginRLS, so no other file reaches the pool primitive either.
func TestArch_TransactionsOpenedOnlyWhereAllowed(t *testing.T) {
	got := map[string]int{}
	for _, f := range sourceFiles(t) {
		if n := countCalls(f.ast, methodCall("BeginTx", "BeginRLS", "openTx")); n > 0 {
			got[f.path] = n
		}
	}
	checkRatchet(t, "BeginTx/BeginRLS/openTx calls", got, map[string]int{
		"internal/storage/database/rls.go":                 4, // openTx's BeginTx, its callers inTx and BeginRLS, the lazy scope
		"internal/storage/database/postgres_migrations.go": 2, // one transaction per step, up and down
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
const handlerBackendCallAllowance = 0

// TestArch_RoutesUseTheTypedAdapter: new endpoints are built with api.JSON or api.Query, which give them a typed
// request, Validate, apperr errors and flow authorization (docs/architecture/api-conventions.md). The routes still
// registered with a hand-written handler are pinned per file. They move over when they are touched, and a new file
// starts at zero.
func TestArch_RoutesUseTheTypedAdapter(t *testing.T) {
	got := map[string]int{}
	for _, f := range sourceFiles(t) {
		if path.Dir(f.path) != "internal/api" {
			continue
		}
		n := 0
		ast.Inspect(f.ast, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if h := routeHandler(call); h != nil && !builtByAdapter(h) {
					n++
				}
			}
			return true
		})
		if n > 0 {
			got[f.path] = n
		}
	}
	checkRatchet(t, "routes with a hand-written handler", got, handWrittenRoutes)
}

// handWrittenRoutes is the number of route registrations per file whose handler is not built by api.JSON or
// api.Query. routes_chi.go's are the router's own (Swagger UI, WebSocket, metrics, pprof, SSE) and stay.
var handWrittenRoutes = map[string]int{
	"internal/api/analysis.go":          8,
	"internal/api/analysis_findings.go": 9,
	"internal/api/analysis_rules.go":    5,
	"internal/api/auth.go":              16,
	// The avatar upload (multipart) and the avatar serving route (a blob download) — the two classes the typed
	// adapter does not fit; the avatar removal beside them is a typed Query endpoint.
	"internal/api/auth_avatar.go":           2,
	"internal/api/flow.go":                  12,
	"internal/api/flow_blocks.go":           7,
	"internal/api/flow_fixes.go":            3,
	"internal/api/flow_source.go":           6,
	"internal/api/handlers_admin.go":        8,
	"internal/api/handlers_apitoken.go":     3,
	"internal/api/handlers_chat.go":         10,
	"internal/api/handlers_comments.go":     6,
	"internal/api/handlers_dashboard.go":    2,
	"internal/api/handlers_export.go":       5,
	"internal/api/handlers_governance.go":   5,
	"internal/api/handlers_orgrules.go":     2,
	"internal/api/handlers_policy.go":       6,
	"internal/api/handlers_providers.go":    12,
	"internal/api/handlers_sharing.go":      4,
	"internal/api/handlers_sso.go":          7,
	"internal/api/handlers_suppressions.go": 1,
	"internal/api/handlers_system.go":       15,
	"internal/api/handlers_triage.go":       8,
	"internal/api/library.go":               7,
	"internal/api/library_versions.go":      5,
	"internal/api/org.go":                   5,
	"internal/api/org_azure.go":             15,
	"internal/api/org_knowledge.go":         4,
	"internal/api/org_members.go":           6,
	"internal/api/providers_azure.go":       14,
	"internal/api/routes_chi.go":            6,
}

// routeHandler returns the handler argument of a route registration and nil for any other call. A registration is
// rb.Get, Post, Put, Patch, Delete or Handle with a literal "/…" pattern, or rb.Method(method, "/…", handler).
func routeHandler(call *ast.CallExpr) ast.Expr {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	at := 0
	switch sel.Sel.Name {
	case "Get", "Post", "Put", "Patch", "Delete", "Handle":
	case "Method":
		at = 1
	default:
		return nil
	}
	if len(call.Args) < at+2 {
		return nil
	}
	lit, ok := call.Args[at].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return nil
	}
	if pattern, err := strconv.Unquote(lit.Value); err != nil || !strings.HasPrefix(pattern, "/") {
		return nil
	}
	return call.Args[at+1]
}

// builtByAdapter reports whether handler is a call of api.JSON or api.Query, with or without type arguments.
func builtByAdapter(handler ast.Expr) bool {
	call, ok := handler.(*ast.CallExpr)
	if !ok {
		return false
	}
	fun := call.Fun
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	var name string
	switch f := fun.(type) {
	case *ast.Ident:
		name = f.Name
	case *ast.SelectorExpr:
		if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name == "api" {
			name = f.Sel.Name
		}
	}
	return name == "JSON" || name == "Query"
}

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
		"internal/ai/httpclient.go":     1,
		"internal/ai/providerconfig.go": 1,
	})
}
