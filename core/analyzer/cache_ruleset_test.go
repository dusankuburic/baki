package analyzer

import (
	"context"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// customRuleSet compiles one CustomRuleConfig into a rule slice for a test.
func customRuleSet(t *testing.T, cfg CustomRuleConfig) []Rule {
	t.Helper()
	r, err := NewCustomRule(cfg)
	if err != nil {
		t.Fatalf("NewCustomRule(%s): %v", cfg.ID, err)
	}
	return []Rule{r}
}

// TestCachedAnalysis_RuleSetParticipatesInKey is the guard for org-scoped custom
// rules.
//
// The cache is keyed on StableFlowID(doc) plus
// analyzerVersion + FlowHash(doc) + settingsDigest(settings). None of those
// depend on WHICH RULES RAN:
//
//   - StableFlowID falls through to the parser's StableID for path-less docs
//     (uploads, /api/analysis/analyze-raw), and StableID is a hash of the FILE
//     NAMES ONLY — deliberately not content, so an edited re-upload keeps one
//     identity. Two tenants both uploading `Main.txt` — the default name PAD
//     exports — therefore share an id.
//   - FlowHash is content-derived, so identical content collides too.
//   - settingsDigest folds in settings.Analysis.Rules, but custom rules are NOT
//     in that map; they arrive as a separate []Rule appended to the slice.
//
// While custom rules are deployment-global that collision is a legitimate
// dedup: the report would be identical anyway. The moment rules become per-org
// it is a cross-tenant leak — org A's custom-rule findings served from cache to
// org B, with nothing in the existing tests to catch it.
//
// This test pins the property the feature depends on: same document, different
// rule set, different report.
func TestCachedAnalysis_RuleSetParticipatesInKey(t *testing.T) {
	// Same file name AND same content in both tenants — the colliding case.
	const src = "#Region \"Main\"\n    SET Token TO 'abc'\n#EndRegion\n"
	const name = "Main.txt"

	// ParseFiles, not ParseText: the upload / analyze-raw path is the one that
	// mints the name-derived StableID, and it is the path two tenants share.
	docA, err := parser.ParseFiles(map[string]string{name: src}, "Main")
	if err != nil {
		t.Fatalf("parse A: %v", err)
	}
	docB, err := parser.ParseFiles(map[string]string{name: src}, "Main")
	if err != nil {
		t.Fatalf("parse B: %v", err)
	}

	// Sanity: the two tenants really do collide on cache identity. If this ever
	// stops being true the test above it is no longer testing anything.
	if got, want := StableFlowID(docA), StableFlowID(docB); got != want {
		t.Fatalf("precondition failed: the two docs no longer share a cache id (%s vs %s)", got, want)
	}
	if got, want := FlowHash(docA), FlowHash(docB); got != want {
		t.Fatalf("precondition failed: the two docs no longer share a content hash (%s vs %s)", got, want)
	}

	// Org A bans SET; org B has no custom rules at all.
	orgARules := append(AllRules(), customRuleSet(t, CustomRuleConfig{
		ID:           "org-a-no-set",
		Name:         "Org A bans SET",
		Description:  "Org A's own policy",
		Severity:     "error",
		Category:     "Style",
		RawTypeMatch: "^SET$",
	})...)
	orgBRules := AllRules()

	DefaultCache.Clear()
	defer DefaultCache.Clear()

	reportA := CachedAnalysisCtx(context.Background(), docA, orgARules, nil, nil)
	reportB := CachedAnalysisCtx(context.Background(), docB, orgBRules, nil, nil)

	nA := 0
	for _, f := range reportA.Findings {
		if f.RuleID == "org-a-no-set" {
			nA++
		}
	}
	nB := 0
	for _, f := range reportB.Findings {
		if f.RuleID == "org-a-no-set" {
			nB++
		}
	}

	if nA == 0 {
		t.Fatalf("org A's own custom rule produced no findings — the fixture does not exercise the rule")
	}
	if nB != 0 {
		t.Errorf("org B received %d finding(s) from org A's custom rule %q — a cached report crossed tenants because the rule set is not part of the cache key", nB, "org-a-no-set")
	}
}

// TestCachedAnalysis_SameRuleIDDifferentConfig covers the subtler half: user
// rule IDs have no global namespace, so two orgs can define DIFFERENT rules
// under the SAME id. Digesting only the ID would collapse them onto one cache
// entry and serve org B the behaviour org A configured.
func TestCachedAnalysis_SameRuleIDDifferentConfig(t *testing.T) {
	const src = "#Region \"Main\"\n    SET Token TO 'abc'\n    WAIT 5\n#EndRegion\n"
	const name = "Main.txt"

	docA, err := parser.ParseFiles(map[string]string{name: src}, "Main")
	if err != nil {
		t.Fatalf("parse A: %v", err)
	}
	docB, err := parser.ParseFiles(map[string]string{name: src}, "Main")
	if err != nil {
		t.Fatalf("parse B: %v", err)
	}

	// Same ID, different matcher: A flags SET, B flags WAIT.
	rulesA := customRuleSet(t, CustomRuleConfig{
		ID: "house-style", Name: "House style", Severity: "warning",
		Category: "Style", RawTypeMatch: "^SET$",
	})
	rulesB := customRuleSet(t, CustomRuleConfig{
		ID: "house-style", Name: "House style", Severity: "warning",
		Category: "Style", RawTypeMatch: "^WAIT$",
	})

	DefaultCache.Clear()
	defer DefaultCache.Clear()

	reportA := CachedAnalysisCtx(context.Background(), docA, rulesA, nil, nil)
	reportB := CachedAnalysisCtx(context.Background(), docB, rulesB, nil, nil)

	blockOf := func(r *models.AnalysisReport) []string {
		var out []string
		for _, f := range r.Findings {
			if f.RuleID == "house-style" {
				out = append(out, f.BlockID)
			}
		}
		return out
	}
	a, b := blockOf(reportA), blockOf(reportB)
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("fixture does not exercise both rules (A=%d, B=%d findings)", len(a), len(b))
	}
	if len(a) == len(b) && a[0] == b[0] {
		t.Errorf("both orgs got the same finding for rule %q — the rule CONFIG is not part of the cache key, so a same-named rule collapsed two tenants onto one entry", "house-style")
	}
}
