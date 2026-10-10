package archtest

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
)

// providerIDs are the registered provider ids, read from the registry (internal/ai/spec_*.go) so a new spec is
// counted the moment it registers; the ratchet below counts them as string literals. "local" and "claude" double
// as a deployment mode and an Azure model kind, so a pinned file is not always a provider branch — the pin says
// how many such lines it has, nothing more.
func providerIDs() []string {
	specs := ai.Specs()
	ids := make([]string, len(specs))
	for i, s := range specs {
		ids[i] = s.ID
	}
	return ids
}

// providerLiteralHome: the files a provider id may be spelled in — the specs (spec.go's wire families share
// two names with them), the catalogue it indexes, and
// config's copy of the embedding-capable ids (config cannot import ai; TestEmbeddingProviderIDs_MatchRegistry
// keeps it honest). core/models/settings.go's default map is the same kind of copy, in the other module.
func providerLiteralHome(path string) bool {
	return strings.HasPrefix(path, "internal/ai/spec") || path == "internal/ai/catalog.go" || path == "internal/config/config.go"
}

// TestArch_ProviderIDLiteralsOutsideSpecs: a provider is described once, in its ai.ProviderSpec, and every list
// is derived from the registry — code that branches on a provider id by name is what the specs replace. Today's
// branch lines are pinned per file; new code asks the spec (Caps, Keys, Modes, Listing) instead of adding one.
func TestArch_ProviderIDLiteralsOutsideSpecs(t *testing.T) {
	exempt := []string{"internal/storage/contract/", "internal/testutil/"}
	ids := providerIDs()
	if len(ids) < 9 {
		t.Fatalf("the registry lists %d providers; the ratchet expects the nine shipped ones at least", len(ids))
	}
	got := map[string]int{}
	for _, f := range sourceFiles(t) {
		skip := providerLiteralHome(f.path)
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
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, id := range ids {
				if v == id {
					n++
				}
			}
			return true
		})
		if n > 0 {
			got[f.path] = n
		}
	}
	checkRatchet(t, "provider id literals outside the provider specs", got, map[string]int{
		"internal/ai/azure_claude.go":                   1,
		"internal/ai/azure_foundry_config.go":           1,
		"internal/ai/azure_registry.go":                 1,
		"internal/ai/claude.go":                         1,
		"internal/ai/copilot.go":                        2,
		"internal/ai/glm.go":                            1,
		"internal/ai/local_provider.go":                 1,
		"internal/ai/openai.go":                         1,
		"internal/ai/stream.go":                         6,
		"internal/ai/xai.go":                            1,
		"internal/api/handlers_system.go":               2,
		"internal/config/loader.go":                     1,
		"internal/di/api.go":                            1,
		"internal/rag/service.go":                       1,
		"internal/storage/interfaces/azure_registry.go": 1,
	})
}

// TestArch_RegisterProviderOnlyInTests: ai.RegisterProvider swaps a provider for a test; production code never
// adds a provider after init — a provider is a spec file.
func TestArch_RegisterProviderOnlyInTests(t *testing.T) {
	for _, f := range sourceFiles(t) {
		n := countCalls(f.ast, func(fun ast.Expr) bool {
			switch fn := fun.(type) {
			case *ast.SelectorExpr:
				return fn.Sel.Name == "RegisterProvider"
			case *ast.Ident:
				return fn.Name == "RegisterProvider"
			}
			return false
		})
		if n > 0 {
			t.Errorf("%s calls RegisterProvider %d times; it is for tests only — add a spec_<id>.go instead", f.path, n)
		}
	}
}
