package di

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"pad-core/cache"

	"pad-analyzer/internal/api"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/service"
)

var updateSubscribers = flag.Bool("update-subscribers", false, "rewrite testdata/subscribers.golden")

// productionSubscribers registers every subscriber the app registers, through the same functions and in the order
// the fx graph invokes them (the services module, then the API module), on a bus of its own. The services behind
// them are nil: subscribing only captures them, and no event is published.
func productionSubscribers() *eventbus.Bus {
	bus := eventbus.New(nil, 0)
	SubscribeFlowCaches(bus, nil, nil, nil)
	SubscribeSettingsAndCredentialCaches(bus, &service.BackendSettingsProvider{}, nil, nil, nil)
	api.SubscribeLentCredentialRevocation(bus, nil, nil)
	SubscribeRealtime(bus, &api.Router{}, nil, nil)
	return bus
}

// TestSubscribers_Golden lists every event subscriber the app registers: its event, tier, the deliveries it runs
// in, its name, and whether other replicas reproduce its effect (replicated, with the invalidation keys it
// publishes) or it stays local (with why). The locality is a required choice of eventbus.Subscribe's options, so a
// new subscriber cannot appear here without one, and a change of a subscriber's locality shows in the diff. Review
// it like a route-policy change; refresh only for an intended one:
// go test ./internal/di/ -run TestSubscribers_Golden -update-subscribers
func TestSubscribers_Golden(t *testing.T) {
	var b strings.Builder
	for _, s := range productionSubscribers().Subscribers() {
		b.WriteString(s.String() + "\n")
	}
	got := b.String()
	path := filepath.Join("testdata", "subscribers.golden")
	if *updateSubscribers {
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
		t.Fatalf("read golden: %v (create it with -update-subscribers)", err)
	}
	if want := strings.ReplaceAll(string(raw), "\r\n", "\n"); got != want {
		t.Errorf("event subscribers changed; review, then refresh with -update-subscribers.\n--- golden\n%s--- now\n%s", want, got)
	}
}

// A subscriber registered without Local or Replicated is not in the golden's vocabulary: it fails here, naming it.
func TestSubscribers_EveryOneChoseItsLocality(t *testing.T) {
	for _, s := range productionSubscribers().Unclassified() {
		t.Errorf("subscriber %q of %s registered without eventbus.Local(reason) or eventbus.Replicated(...)", s.Name, s.Event)
	}
}

// Every key a replicated subscriber publishes names a cache declared with cache.Declare, or a recheck. A cache
// named by a typo would be invalidated nowhere.
func TestSubscribers_ReplicatedKeysNameDeclaredCachesOrRechecks(t *testing.T) {
	declared := map[string]bool{}
	for _, s := range cache.Specs() {
		declared[s.Name] = true
	}
	for _, key := range productionSubscribers().ReplicatedKeys() {
		if !declared[key] && !strings.HasPrefix(key, "recheck.") {
			t.Errorf("replicated key %q is neither a declared cache (pad-core/cache.Declare) nor a recheck.*", key)
		}
	}
}

// Every replicated key has its applier (Bus.OnRemote: the cache owner's invalidate, or the sockets' recheck) next to
// its subscriber, so a message for it is never silently ignored. A new replicated subscriber registers its own.
func TestSubscribers_EveryReplicatedKeyHasAnApplier(t *testing.T) {
	if got := productionSubscribers().MissingAppliers(); len(got) != 0 {
		t.Errorf("replicated keys without an OnRemote applier = %v: register the applier next to the subscriber", got)
	}
}

// A replicated cache key is a cache declared Shared by the invalidation fan-out, and a cache declared that way has a
// replicated key: the caches golden and the subscribers golden cannot disagree about what crosses replicas.
func TestSubscribers_ReplicatedCachesAreSharedAndSharedCachesAreReplicated(t *testing.T) {
	replicated := map[string]bool{}
	for _, k := range productionSubscribers().ReplicatedKeys() {
		replicated[k] = true
	}
	for _, s := range cache.Specs() {
		shared := s.Locality == cache.LocalityShared && s.Mechanism == cache.MechanismInvalidation
		if replicated[s.Name] && !shared {
			t.Errorf("cache %q is invalidated across replicas but declared %s: declare it cache.Shared(cache.MechanismInvalidation, reason)", s.Name, s.Locality)
		}
		if shared && !replicated[s.Name] {
			t.Errorf("cache %q is declared shared by the invalidation fan-out but no replicated subscriber publishes its key", s.Name)
		}
		if s.Locality == cache.LocalityShared && s.Mechanism != cache.MechanismInvalidation {
			t.Errorf("cache %q is shared by %q: Typed storage can only be shared by the invalidation fan-out", s.Name, s.Mechanism)
		}
	}
}

// knownRegistrars maps each function that calls eventbus.Subscribe (package directory and name) to the subscribers
// it registers. productionSubscribers must call every one, or the golden would miss them.
var knownRegistrars = map[string][]string{
	"internal/di.SubscribeFlowCaches":                {"analyzer-report-cache", "chat-context-cache", "dashboard-cache", "flow-document-cache", "flow-search-index"},
	"internal/di.SubscribeRealtime":                  {"finding-assignment-fanout", "finding-comment-fanout", "ws-flow-changed"},
	"internal/service.SubscribeSettingsCache":        {"settings-cache"},
	"internal/service.SubscribeRuleProfiles":         {"rule-profiles"},
	"internal/service.SubscribeModelCache":           {"provider-model-cache"},
	"internal/rag.SubscribeCorpusCache":              {"knowledge-corpus-count"},
	"internal/websocket.SubscribeAccessRechecks":     {"ws-access-recheck"},
	"internal/api.SubscribeRevocations":              {"sse-access-revoked"},
	"internal/api.SubscribeLentCredentialRevocation": {"lent-credential-revocation"},
}

// TestSubscribers_EverySubscribeCallIsExercisedByTheGolden: every eventbus.Subscribe call outside tests sits in a
// registrar productionSubscribers drives, and passes an option (the locality; the golden checks which). A
// subscriber registered somewhere new is not in the golden, so it could skip the locality review; this fails
// until the registrar is listed here and productionSubscribers calls it.
func TestSubscribers_EverySubscribeCallIsExercisedByTheGolden(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			pkg := eventbusImportName(f)
			if pkg == "" {
				return nil
			}
			rel, _ := filepath.Rel(root, filepath.Dir(p))
			rel = filepath.ToSlash(rel)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || !isSubscribeCall(call, pkg) {
						return true
					}
					id := rel + "." + fn.Name.Name
					found[id] = true
					if _, known := knownRegistrars[id]; !known {
						t.Errorf("%s calls eventbus.Subscribe but is not in knownRegistrars: add it, with its subscribers' names, and make productionSubscribers call it", id)
					}
					if len(call.Args) < 5 && call.Ellipsis == token.NoPos {
						t.Errorf("%s: a Subscribe call without options has no locality (eventbus.Local or eventbus.Replicated)", id)
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	registered := map[string]bool{}
	for _, s := range productionSubscribers().Subscribers() {
		registered[s.Name] = true
	}
	var want []string
	for id, names := range knownRegistrars {
		if !found[id] {
			t.Errorf("knownRegistrars lists %s, which no longer calls eventbus.Subscribe: remove it", id)
		}
		want = append(want, names...)
	}
	sort.Strings(want)
	var got []string
	for n := range registered {
		got = append(got, n)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("productionSubscribers registers %v, but knownRegistrars lists %v", got, want)
	}
}

// eventbusImportName is the name f imports the event bus package under ("" when it does not).
func eventbusImportName(f *ast.File) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "pad-analyzer/internal/platform/eventbus" {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return "eventbus"
		}
	}
	return ""
}

// isSubscribeCall matches pkg.Subscribe(...) and pkg.Subscribe[E](...).
func isSubscribeCall(call *ast.CallExpr, pkg string) bool {
	fun := call.Fun
	if ix, ok := fun.(*ast.IndexExpr); ok {
		fun = ix.X
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Subscribe" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg
}
