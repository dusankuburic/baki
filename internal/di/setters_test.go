package di

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// diSetters is every post-construction Set*/On* call the DI wiring makes, keyed by the call's selector, with
// why it is not a constructor parameter. docs/architecture/di-wiring.md explains each; keep the two in step.
// A dependency without a cycle belongs in the constructor; a cache-invalidation hook waits for Phase 2b's
// event bus.
var diSetters = map[string]string{
	// Construction-time configuration of the factory ProvideAI itself builds (ai.NewProviderFactory's
	// signature is shared by ~65 test call sites).
	"factory.SetEmbeddingDimensions": "configures the object its own provider builds",
	"factory.SetOrgKeyLookup":        "configures the object its own provider builds",
	// Real cycle: the storage backend's data-subject export reads Azure connection configs from the
	// AzureModelService, which is built on the backend. Phase 3's data-lifecycle registry replaces it.
	"src.SetAzureExportConfigSource": "cycle: backend <-> AzureModelService (Phase 3)",
	// Event hooks Phase 2b's bus replaces.
	"flowSvc.SetCloudFlowMutationHook": "event hook (Phase 2b): FlowService -> dashboard cache",
	"flowSvc.OnInvalidateFlow":         "event hook (Phase 2b): FlowService -> chat context cache",
	"sys.OnApiKeyChanged":              "event hook (Phase 2b): SystemService -> provider model cache",
	"svc.OnUserDeleted":                "event hook (Phase 2b): account erasure (AccountLifecycleService) -> provider caches",
	// No cycle: NewFlowService has ~55 test call sites, so the resolver is decorated in.
	"svc.SetRuleProfileResolver": "no cycle; FlowService constructor churn (deferred)",
}

var setterName = regexp.MustCompile(`^(Set|On)[A-Z]`)

// TestDISetters_Listed: a new post-construction setter in the DI wiring fails until it is listed in diSetters
// with its reason (and documented); a removed one fails until its entry goes, so the list stays exact.
func TestDISetters_Listed(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && setterName.MatchString(sel.Sel.Name) {
				seen[types.ExprString(sel)]++
			}
			return true
		})
	}
	var problems []string
	for call, n := range seen {
		if _, ok := diSetters[call]; !ok {
			problems = append(problems, "unlisted post-construction setter "+call+": make it a constructor parameter, or list it in diSetters and docs/architecture/di-wiring.md with the reason")
		} else if n > 1 {
			problems = append(problems, "setter "+call+" is called more than once; list each call site separately")
		}
	}
	for call := range diSetters {
		if seen[call] == 0 {
			problems = append(problems, "listed setter "+call+" is no longer called: remove it from diSetters and docs/architecture/di-wiring.md")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}
