package api

import (
	"go/types"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/testutil"
)

// billedProviderPkg declares the provider chain: a method named Chat, Stream or Embed on one of its types is a
// billed provider call (the audited wrapper, the retry and breaker layers, every vendor client).
const billedProviderPkg = "pad-analyzer/internal/ai"

// notBilledDespiteCallGraph lists the routes the call graph says can reach a billed provider call, but cannot,
// each with the reason. The analysis is object-insensitive: every value stored in one func-typed field shares one
// set of targets. Each entry must still be flagged (else it is stale) and must not be declared billed.
var notBilledDespiteCallGraph = map[string]string{
	"POST /api/admin/migration/start": "the data migration runs as a lifecycle.Singleton job, whose dispatch (s.job.Run) the " +
		"call graph cannot tell from the knowledge re-embed job's; internal/migration imports neither ai nor rag",
}

// TestBilledRoutes_MatchTheCallGraph (R-security-c22): a route is declared billed (route.Billed) exactly when its
// handler can reach a billed provider call (ai.Provider Chat, Stream or Embed), derived from the module's call
// graph (SSA, refined by variable type analysis) rather than a hand-kept list, and every billed route resolves to
// a cost bucket, never the general one. A new handler that reaches a provider fails here until it is marked billed.
func TestBilledRoutes_MatchTheCallGraph(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks and builds the call graph of the whole module (~15 s)")
	}
	t.Parallel()
	g := loadBilledCallGraph(t)
	rt := newTestRouter(testutil.NewFakeBackend(), true)

	seenExempt := map[string]bool{}
	var billed []string
	for _, p := range rt.routes.Policies() {
		key := p.Method + " " + p.Pattern
		name, fns := g.handlerFunctions(rt.routes.Endpoint(p))
		if len(fns) == 0 {
			t.Errorf("%s: handler %s not found in the call graph; teach handlerFunctions its shape", key, name)
			continue
		}
		reaches := false
		for _, fn := range fns {
			reaches = reaches || g.reaches[fn]
		}
		if reason, ok := notBilledDespiteCallGraph[key]; ok {
			seenExempt[key] = true
			if !reaches {
				t.Errorf("%s is listed as a call-graph false positive (%s) but no longer reaches a provider: drop the entry", key, reason)
			}
			if p.Billed {
				t.Errorf("%s is declared billed and listed as not billed", key)
			}
			continue
		}
		switch {
		case reaches && !p.Billed:
			t.Errorf("%s (%s) can reach a billed provider call: register it with route.Billed() on a cost bucket", key, name)
		case !reaches && p.Billed:
			t.Errorf("%s is declared billed, but its handler %s reaches no provider call: drop route.Billed()", key, name)
		}
		if p.Billed {
			billed = append(billed, key)
			if !route.CostBucket(p.RateLimit) {
				t.Errorf("%s is billed on the %q bucket: a billed route rides a cost bucket", key, p.RateLimit)
			}
		}
	}
	for key := range notBilledDespiteCallGraph {
		if !seenExempt[key] {
			t.Errorf("notBilledDespiteCallGraph lists %s, which is not a route", key)
		}
	}
	// The routes the roadmap item names, so an analysis that flags nothing cannot pass.
	for _, want := range []string{
		"POST /api/chat/stream", "POST /api/chat/preview-context", "POST /api/providers/test",
		"POST /api/providers/azure-foundry/test", "POST /api/orgs/{id}/providers/azure-foundry/test",
		"POST /api/orgs/{id}/knowledge/upload", "POST /api/orgs/{id}/knowledge/reindex",
	} {
		if !slices.Contains(billed, want) {
			t.Errorf("%s is not billed; billed routes: %v", want, billed)
		}
	}
}

// billedCallGraph is the module's call graph reduced to what the test asks of it.
type billedCallGraph struct {
	// reaches holds every function from which a billed provider call is reachable.
	reaches map[*ssa.Function]bool
	// byName indexes the named functions and methods by their runtime name (runtimeName).
	byName map[string]*ssa.Function
	// family maps a named function's runtime name to it, its closures and its generic instantiations.
	family map[string][]*ssa.Function
	// familyByShort maps a family's name without its package path (Func, (*T).Method) to the families' names: an
	// inlined closure's runtime name carries the package it was inlined into, not its own.
	familyByShort map[string][]string
}

func loadBilledCallGraph(t *testing.T) *billedCallGraph {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repo root")
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports |
			packages.NeedTypes | packages.NeedTypesSizes | packages.NeedSyntax | packages.NeedTypesInfo,
		Dir: filepath.Join(filepath.Dir(here), "..", ".."),
	}
	pkgs, err := packages.Load(cfg, "pad-analyzer/internal/...")
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("the module does not type-check")
	}
	prog, _ := ssautil.Packages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	all := ssautil.AllFunctions(prog)
	cg := vta.CallGraph(all, cha.CallGraph(prog))

	g := &billedCallGraph{
		reaches: map[*ssa.Function]bool{}, byName: map[string]*ssa.Function{},
		family: map[string][]*ssa.Function{}, familyByShort: map[string][]string{},
	}
	var queue []*callgraph.Node
	for fn, n := range cg.Nodes {
		if fn != nil && fn.Pkg != nil && fn.Pkg.Pkg.Path() == billedProviderPkg && fn.Signature.Recv() != nil {
			switch fn.Name() {
			case "Chat", "Stream", "Embed":
				g.reaches[fn] = true
				queue = append(queue, n)
			}
		}
	}
	if len(queue) == 0 {
		t.Fatal("no provider Chat/Stream/Embed method in the call graph")
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, e := range n.In {
			if f := e.Caller.Func; f != nil && !g.reaches[f] {
				g.reaches[f] = true
				queue = append(queue, e.Caller)
			}
		}
	}
	for fn := range all {
		root := fn
		for root.Parent() != nil {
			root = root.Parent()
		}
		if o := root.Origin(); o != nil {
			root = o
		}
		name := runtimeName(root)
		if root == fn {
			g.byName[name] = fn
		}
		if _, seen := g.family[name]; !seen {
			_, short := splitFuncName(name)
			g.familyByShort[short] = append(g.familyByShort[short], name)
		}
		g.family[name] = append(g.family[name], fn)
	}
	return g
}

// splitFuncName splits a runtime function name into its package path and the rest (Func, (*T).Method, T.Method).
func splitFuncName(name string) (pkg, rest string) {
	slash := strings.LastIndexByte(name, '/')
	dot := strings.IndexByte(name[slash+1:], '.')
	if dot < 0 {
		return "", name
	}
	return name[:slash+1+dot], name[slash+1+dot+1:]
}

// runtimeName spells a named function as runtime.FuncForPC does: pkg.Func, pkg.(*T).Method, pkg.T.Method, a generic
// one with its type arguments as [...].
func runtimeName(fn *ssa.Function) string {
	pkg := ""
	if fn.Pkg != nil {
		pkg = fn.Pkg.Pkg.Path()
	} else if obj := fn.Object(); obj != nil && obj.Pkg() != nil {
		pkg = obj.Pkg().Path()
	}
	name := fn.Name()
	if i := strings.IndexByte(name, '['); i >= 0 {
		name = name[:i]
	}
	if fn.TypeParams().Len() > 0 || len(fn.TypeArgs()) > 0 {
		name += "[...]"
	}
	recv := fn.Signature.Recv()
	if recv == nil {
		return pkg + "." + name
	}
	typ, ptr := recv.Type(), false
	if p, ok := typ.(*types.Pointer); ok {
		typ, ptr = p.Elem(), true
	}
	tn := ""
	if n, ok := typ.(*types.Named); ok {
		tn = n.Obj().Name()
		if n.TypeArgs().Len() > 0 || n.TypeParams().Len() > 0 {
			tn += "[...]"
		}
	}
	if ptr {
		return pkg + ".(*" + tn + ")." + name
	}
	return pkg + "." + tn + "." + name
}

// closureSuffix matches the closure segments the runtime appends to a function's name (.func1, .func1.2, .gowrap1).
var closureSuffix = regexp.MustCompile(`(\.(func|gowrap|deferwrap)?\d+)+$`)

// handlerFunctions returns a route handler's runtime name and the functions it runs: the method or function itself
// for a method value or a named function; for a closure, its enclosing named function's family (that function, its
// closures and its instantiations: a conservative superset, since closure numbering and inlining differ between the
// runtime's names and SSA's). An http.Handler that is not a func runs its ServeHTTP method.
func (g *billedCallGraph) handlerFunctions(h http.Handler) (string, []*ssa.Function) {
	if hf, ok := h.(http.HandlerFunc); ok {
		name := strings.TrimSuffix(runtime.FuncForPC(reflect.ValueOf(hf).Pointer()).Name(), "-fm")
		if fn, ok := g.byName[name]; ok {
			return name, []*ssa.Function{fn}
		}
		return name, g.enclosingFamily(closureSuffix.ReplaceAllString(name, ""))
	}
	typ := reflect.TypeOf(h)
	ptr := typ.Kind() == reflect.Pointer
	if ptr {
		typ = typ.Elem()
	}
	name := typ.PkgPath() + "." + typ.Name() + ".ServeHTTP"
	if ptr {
		name = typ.PkgPath() + ".(*" + typ.Name() + ").ServeHTTP"
	}
	if fn, ok := g.byName[name]; ok {
		return name, []*ssa.Function{fn}
	}
	return name, nil
}

// enclosingFamily finds the named function a closure's runtime name ends in. Inlining prefixes the name with the
// functions it was inlined into, under their package (api.(*Router).Mount.MetricsGuard for middleware.MetricsGuard),
// so the longest suffix that names a function wins: of the name's package, else of any package (every such family).
func (g *billedCallGraph) enclosingFamily(name string) []*ssa.Function {
	pkg, rest := splitFuncName(name)
	if pkg == "" {
		return nil
	}
	for {
		var fns []*ssa.Function
		if own, ok := g.family[pkg+"."+rest]; ok {
			fns = append(fns, own...)
		} else {
			for _, other := range g.familyByShort[rest] {
				fns = append(fns, g.family[other]...)
			}
		}
		if len(fns) > 0 {
			sort.Slice(fns, func(i, j int) bool { return fns[i].String() < fns[j].String() })
			return fns
		}
		i := strings.IndexByte(rest, '.')
		if i < 0 {
			return nil
		}
		rest = rest[i+1:]
	}
}
