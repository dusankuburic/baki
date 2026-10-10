package metrics

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// centralListPin is len(onBehalf): the collectors this package still registers for other packages. It only goes
// down: a metric moves to the package that records it (metrics.MustRegister from a package-level var or init), and
// a new metric registers there from the start.
const centralListPin = 9

func TestMetrics_CentralListOnlyShrinks(t *testing.T) {
	switch n := len(onBehalf); {
	case n > centralListPin:
		t.Errorf("onBehalf has %d collectors, pinned at %d: register a new metric from the package that records it (metrics.MustRegister)", n, centralListPin)
	case n < centralListPin:
		t.Errorf("onBehalf is down to %d: lower centralListPin to %d", n, n)
	}
}

func TestModuleRegistry_RecordsTheRegisteringPackage(t *testing.T) {
	r := newModuleRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_registrations_total", Help: "A test counter."})
	r.MustRegister(c)
	regs := r.registrations()
	if len(regs) != 1 || regs[0].collector != c || regs[0].pkg != modulePath+"/internal/metrics" {
		t.Fatalf("registrations = %+v", regs)
	}
	// A second registration of the same metric fails and records nothing.
	err := r.register(modulePath+"/internal/metrics", []prometheus.Collector{c})
	var are prometheus.AlreadyRegisteredError
	if !errors.As(err, &are) {
		t.Errorf("duplicate registration: err = %v", err)
	}
	if len(r.registrations()) != 1 {
		t.Errorf("a refused collector was recorded: %+v", r.registrations())
	}
	defer func() {
		if recover() == nil {
			t.Error("MustRegister of a duplicate did not panic")
		}
	}()
	r.MustRegister(c)
}

func TestPackageOf(t *testing.T) {
	for fn, want := range map[string]string{
		"pad-analyzer/internal/metrics.init.0":                     "pad-analyzer/internal/metrics",
		"pad-analyzer/internal/metrics.(*moduleRegistry).register": "pad-analyzer/internal/metrics",
		"pad-analyzer/internal/storage/database.glob..func1":       "pad-analyzer/internal/storage/database",
		"pad-analyzer/internal/metrics_test.TestX.func2":           "pad-analyzer/internal/metrics_test",
		"pad-analyzer/internal/platform/eventbus.New[...].func1":   "pad-analyzer/internal/platform/eventbus",
		"main.init": "main",
		"github.com/prometheus/client_golang/prometheus.(*Registry)": "github.com/prometheus/client_golang/prometheus",
	} {
		if got := packageOf(fn); got != want {
			t.Errorf("packageOf(%q) = %q, want %q", fn, got, want)
		}
	}
}

// metricsFile is one parsed non-test Go file of the root module.
type metricsFile struct {
	path string // slash path from the repo root
	pkg  string // import path of its package
	ast  *ast.File
}

func rootModuleFiles(t *testing.T) []metricsFile {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var out []metricsFile
	parse := func(p string) {
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		pkg := modulePath + "/" + path.Dir(rel)
		if path.Dir(rel) == "." {
			pkg = "main"
		}
		out = append(out, metricsFile{rel, pkg, f})
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			parse(filepath.Join(root, e.Name()))
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

// importName is the name f refers to the import path by, or "" when f does not import it.
func importName(f *ast.File, importPath string) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == importPath {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return path.Base(importPath)
		}
	}
	return ""
}

// selectorOf returns pkg.Sel when n is a selector on the identifier pkg (pkg != "").
func selectorOf(n ast.Node, pkg string) (string, bool) {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok || pkg == "" {
		return "", false
	}
	if x, ok := sel.X.(*ast.Ident); ok && x.Name == pkg {
		return sel.Sel.Name, true
	}
	return "", false
}

// registrationCall reports whether call registers on the package registry: metrics.Register/MustRegister from
// another package; Register, MustRegister, registry.Register or registry.MustRegister inside this one.
func registrationCall(f metricsFile, metricsName string, call *ast.CallExpr) bool {
	isReg := func(name string) bool { return name == "Register" || name == "MustRegister" }
	if f.pkg != modulePath+"/internal/metrics" {
		name, ok := selectorOf(call.Fun, metricsName)
		return ok && isReg(name)
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		return isReg(id.Name)
	}
	name, ok := selectorOf(call.Fun, "registry")
	return ok && isReg(name)
}

// TestRegistration_OnlyThroughThePackageRegistry: every collector reaches the registry /metrics serves through
// Register or MustRegister, from a package-level var or an init function, in a package this test binary links:
//
//   - nothing uses the client library's global registry (prometheus.MustRegister, promauto, promhttp.Handler…),
//     which /metrics does not serve, or builds a registry of its own;
//   - registration happens at package initialisation, so every metric exists before anything records it and the
//     golden, which never constructs a service, sees it; registering at construction would also fail the second
//     time a test builds the service;
//   - every package that registers is linked into this test binary (registry_external_test.go imports it), so the
//     golden lists its metrics. Package main cannot be linked: register from a package it imports.
func TestRegistration_OnlyThroughThePackageRegistry(t *testing.T) {
	forbidden := map[string]bool{
		"MustRegister": true, "Register": true, "Unregister": true, "DefaultRegisterer": true, "DefaultGatherer": true,
		"NewRegistry": true, "NewPedanticRegistry": true, "WrapRegistererWith": true, "WrapRegistererWithPrefix": true,
	}
	registering := map[string][]string{} // package → files registering at package level
	for _, f := range rootModuleFiles(t) {
		if importName(f.ast, "github.com/prometheus/client_golang/prometheus/promauto") != "" {
			t.Errorf("%s imports promauto, which registers on the global registry /metrics does not serve: use metrics.MustRegister", f.path)
		}
		promName := importName(f.ast, "github.com/prometheus/client_golang/prometheus")
		httpName := importName(f.ast, "github.com/prometheus/client_golang/prometheus/promhttp")
		metricsName := importName(f.ast, modulePath+"/internal/metrics")
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if name, ok := selectorOf(n, promName); ok && forbidden[name] {
				if f.path == "internal/metrics/metrics.go" && name == "NewRegistry" {
					return true // the package registry itself
				}
				t.Errorf("%s uses prometheus.%s: register through metrics.MustRegister on the registry /metrics serves", f.path, name)
			}
			if name, ok := selectorOf(n, httpName); ok && (name == "Handler" || name == "InstrumentMetricHandler") {
				t.Errorf("%s uses promhttp.%s, which serves the global registry: use metrics.Handler", f.path, name)
			}
			if f.path != "internal/metrics/metrics.go" {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "reg" {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "registry" && f.pkg == modulePath+"/internal/metrics" {
						t.Errorf("%s reaches the raw registry: register through MustRegister, which records the package", f.path)
					}
				}
			}
			return true
		})
		// Registration calls: allowed in package-level var initialisers and init functions only.
		for _, decl := range f.ast.Decls {
			atInit := false
			switch d := decl.(type) {
			case *ast.GenDecl:
				atInit = d.Tok == token.VAR
			case *ast.FuncDecl:
				atInit = d.Recv == nil && d.Name.Name == "init"
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !registrationCall(f, metricsName, call) {
					return true
				}
				if !atInit {
					t.Errorf("%s registers a collector outside a package-level var or init function", f.path)
				}
				registering[f.pkg] = append(registering[f.pkg], f.path)
				return true
			})
		}
	}
	linked := map[string]bool{}
	for _, r := range registry.registrations() {
		linked[r.pkg] = true
	}
	var pkgs []string
	for p := range registering {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		files := slices.Compact(registering[p])
		if !linked[p] {
			t.Errorf("%s registers metrics (%s) but registered nothing in this test binary: import it in registry_external_test.go so the golden lists them",
				p, strings.Join(files, ", "))
		}
	}
	if !linked[modulePath+"/internal/metrics"] || len(pkgs) == 0 {
		t.Errorf("the scan found no registration of this package's own (found %v): the walk is broken", pkgs)
	}
}
