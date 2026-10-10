package analyzer

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"pad-core/cache"
	"pad-core/models"
	"pad-core/parser"
)

// rawFlow is a small flow with findings (a hardcoded URL, an unhandled web action) and a suppression directive.
const rawFlow = "#Region \"Main\"\n" +
	"# pad-ignore[hardcoded-url]\n" +
	"WebAutomation.LaunchEdge.LaunchEdge Url: $'''https://intranet.example.com/secret''' BrowserInstance=> Browser\n" +
	"SET Token TO $'''abc'''\n" +
	"#EndRegion\n"

func parseRaw(t *testing.T, src, root string) *models.FlowDocument {
	t.Helper()
	doc, err := parser.ParseFiles(map[string]string{"Main.txt": src}, root)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

// requireOwnBlocks fails unless every finding of r names a block and subflow of doc: the ids an export maps to
// lines and files.
func requireOwnBlocks(t *testing.T, r *models.AnalysisReport, doc *models.FlowDocument) {
	t.Helper()
	if len(r.Findings) == 0 {
		t.Fatal("fixture rot: the flow must produce findings")
	}
	for _, f := range r.Findings {
		if f.BlockID != "" && doc.BlocksByID[f.BlockID] == nil {
			t.Errorf("finding %s (%s) names block %s, which the caller's document does not have", f.ID, f.RuleID, f.BlockID)
		}
		if f.SubflowID != "" && doc.SubflowsByID[f.SubflowID] == nil {
			t.Errorf("finding %s (%s) names subflow %s, which the caller's document does not have", f.ID, f.RuleID, f.SubflowID)
		}
	}
}

// B1: two callers posting the same file names and content to analyze-raw shared the whole key, so the second got
// the first's report object: their root name, document id and block ids.
func TestCachedAnalyzer_CallersDoNotShareReports(t *testing.T) {
	a := newTestAnalyzer()
	ctx := context.Background()
	victim := parseRaw(t, rawFlow, "victim-secret-project")
	attacker := parseRaw(t, rawFlow, "attacker")
	if StableFlowID(victim) != StableFlowID(attacker) || FlowHash(victim) != FlowHash(attacker) {
		t.Fatal("fixture rot: the two parses must share the flow identity and the content hash")
	}
	victimTenant, attackerTenant := cache.Org("victim-org"), cache.User("attacker")

	rv := a.Analyze(ctx, Input{Doc: victim, Rules: AllRules(), Tenant: victimTenant})
	ra := a.Analyze(ctx, Input{Doc: attacker, Rules: AllRules(), Tenant: attackerTenant})

	if ra == rv {
		t.Fatal("the second caller was served the first caller's report")
	}
	if ra.FlowName != attacker.Name || ra.FlowID != attacker.ID {
		t.Errorf("second caller's report names flow %q (%s), want its own %q (%s)", ra.FlowName, ra.FlowID, attacker.Name, attacker.ID)
	}
	requireOwnBlocks(t, ra, attacker)
	if got := a.Reports(attackerTenant); len(got) != 1 || got[0] != ra {
		t.Errorf("Reports(attacker) = %d reports, want only the attacker's own", len(got))
	}
	if got := a.Reports(victimTenant); len(got) != 1 || got[0] != rv {
		t.Errorf("Reports(victim) = %d reports, want only the victim's own", len(got))
	}
	if got := a.Reports(cache.Local()); len(got) != 0 {
		t.Errorf("Reports(local) = %d reports, want none: no local analysis ran", len(got))
	}
}

// B2: within one tenant, a re-parse of unchanged content hit the first parse's report, whose block ids the new
// document does not have (analyze-raw SARIF lost every line number). The same document still hits.
func TestCachedAnalyzer_ReparseGetsItsOwnBlockIDs(t *testing.T) {
	a := newTestAnalyzer()
	ctx := context.Background()
	first := parseRaw(t, rawFlow, "Main")
	second := parseRaw(t, rawFlow, "Main")

	r1 := a.Analyze(ctx, localIn(first, AllRules(), nil, nil))
	if again := a.Analyze(ctx, localIn(first, AllRules(), nil, nil)); again != r1 {
		t.Error("the same document must hit its cached report")
	}
	if a.Lookup(localIn(second, AllRules(), nil, nil)) != nil {
		t.Error("a re-parse of unchanged content hit the first parse's report")
	}
	r2 := a.Analyze(ctx, localIn(second, AllRules(), nil, nil))
	requireOwnBlocks(t, r2, second)
	if r2.ContentHash != r1.ContentHash || r2.ContentHash != FlowHash(second) {
		t.Errorf("ContentHash %q / %q, want FlowHash %q on both", r1.ContentHash, r2.ContentHash, FlowHash(second))
	}
	// One entry per flow: the second parse replaced the first.
	if got := a.Reports(cache.Local()); len(got) != 1 || got[0] != r2 {
		t.Errorf("Reports(local) = %d reports, want the second parse's only", len(got))
	}

	// A whitespace-only edit keeps FlowHash but moves every line (finding fingerprints include the line).
	edited := parseRaw(t, "\n\n\n"+rawFlow, "Main")
	if FlowHash(edited) != FlowHash(first) {
		t.Fatal("fixture rot: blank lines must not change FlowHash")
	}
	re := a.Analyze(ctx, localIn(edited, AllRules(), nil, nil))
	requireOwnBlocks(t, re, edited)
	want := RunAnalysis(edited, AllRules(), nil, nil)
	if len(re.Findings) != len(want.Findings) {
		t.Fatalf("edited flow: %d findings, want %d", len(re.Findings), len(want.Findings))
	}
	for i := range want.Findings {
		if re.Findings[i].Fingerprint != want.Findings[i].Fingerprint {
			t.Errorf("finding %d fingerprint %s, want %s (an uncached analysis of the edited flow)", i, re.Findings[i].Fingerprint, want.Findings[i].Fingerprint)
		}
	}
}

// B3: the inventory was keyed by content alone, which ignores blank lines, so a flow that differs from another
// only by blank lines was served the other flow's line numbers.
func TestCachedAnalyzer_InventoryLinesAreTheCallersOwn(t *testing.T) {
	a := newTestAnalyzer()
	plain := parseRaw(t, rawFlow, "Main")
	shifted := parseRaw(t, "\n\n\n"+rawFlow, "Main")

	first := a.SuppressionInventory(localIn(plain, AllRules(), nil, nil))
	got := a.SuppressionInventory(localIn(shifted, AllRules(), nil, nil))
	want := SuppressionInventory(shifted, AllRules(), nil)
	if len(first) == 0 || len(got) != len(want) {
		t.Fatalf("inventories: first %d entries, shifted %d, uncached %d", len(first), len(got), len(want))
	}
	for i := range want {
		if got[i].Line != want[i].Line {
			t.Errorf("entry %d on line %d, want %d (the caller's own line)", i, got[i].Line, want[i].Line)
		}
	}
	if first[0].Line == got[0].Line {
		t.Error("fixture rot: the two flows must place the directive on different lines")
	}
}

// The inventory is per tenant: another tenant's identical request builds its own.
func TestCachedAnalyzer_InventoryPerTenant(t *testing.T) {
	var calls atomic.Int64
	rules := append(AllRules(), countingRule{&calls})
	a := newTestAnalyzer()
	doc := parseRaw(t, rawFlow, "Main")

	a.SuppressionInventory(Input{Doc: doc, Rules: rules, Tenant: cache.Org("o1")})
	afterFirst := calls.Load()
	a.SuppressionInventory(Input{Doc: doc, Rules: rules, Tenant: cache.Org("o1")})
	if calls.Load() != afterFirst {
		t.Error("the same tenant's repeat was not served from the cache")
	}
	a.SuppressionInventory(Input{Doc: doc, Rules: rules, Tenant: cache.Org("o2")})
	if calls.Load() == afterFirst {
		t.Error("another tenant was served the first tenant's inventory")
	}
}

// A zero tenant analyzes without the cache: nothing is stored, and nothing is served.
func TestCachedAnalyzer_ZeroTenantIsNeverCached(t *testing.T) {
	a := newTestAnalyzer()
	doc := parseRaw(t, rawFlow, "Main")
	in := Input{Doc: doc, Rules: AllRules()}

	r1 := a.Analyze(context.Background(), in)
	r2 := a.Analyze(context.Background(), in)
	if r1 == nil || r2 == nil || r1 == r2 {
		t.Fatalf("zero tenant: reports %p and %p, want two separate walks", r1, r2)
	}
	if r1.ContentHash != FlowHash(doc) {
		t.Errorf("an uncached report must still carry the content hash, got %q", r1.ContentHash)
	}
	if a.Lookup(in) != nil || a.reports.Len() != 0 {
		t.Errorf("zero tenant: %d report(s) stored", a.reports.Len())
	}
	if got := a.SuppressionInventory(in); len(got) == 0 {
		t.Error("zero tenant: the inventory must still be built")
	}
	if a.inventory.Len() != 0 {
		t.Errorf("zero tenant: %d inventories stored", a.inventory.Len())
	}

	// A document without any identity is not cached either.
	anon := &models.FlowDocument{Subflows: doc.Subflows}
	if r := a.Analyze(context.Background(), localIn(anon, AllRules(), nil, nil)); r == nil || a.reports.Len() != 0 {
		t.Errorf("identity-less document: report %v, %d stored", r != nil, a.reports.Len())
	}

	// A nil analyzer analyzes too.
	var none *CachedAnalyzer
	if r := none.Analyze(context.Background(), localIn(doc, AllRules(), nil, nil)); r == nil {
		t.Error("nil analyzer returned no report")
	}
	if none.Lookup(localIn(doc, AllRules(), nil, nil)) != nil || none.Reports(cache.Local()) != nil {
		t.Error("nil analyzer served a report")
	}
	none.InvalidateFlow("x")
	none.ResetTenant(cache.Local())
}

// InvalidateFlow drops the flow's entries of every tenant and nothing else.
func TestCachedAnalyzer_InvalidateFlowDropsEveryTenant(t *testing.T) {
	a := newTestAnalyzer()
	ctx := context.Background()
	stored := parseRaw(t, rawFlow, "Main")
	stored.ID, stored.StableID = "flow-1", "" // a stored flow: its identity is its id
	other := parseRaw(t, rawFlow, "Other")
	other.ID, other.StableID = "flow-2", ""

	a.Analyze(ctx, Input{Doc: stored, Rules: AllRules(), Tenant: cache.Org("o1")})
	a.Analyze(ctx, Input{Doc: stored, Rules: AllRules(), Tenant: cache.User("u1")})
	keep := a.Analyze(ctx, Input{Doc: other, Rules: AllRules(), Tenant: cache.Org("o1")})
	if n := a.reports.Len(); n != 3 {
		t.Fatalf("setup: %d entries, want 3", n)
	}

	a.InvalidateFlow("flow-1")

	if n := a.reports.Len(); n != 1 {
		t.Fatalf("after InvalidateFlow: %d entries, want 1", n)
	}
	if got := a.Reports(cache.Org("o1")); len(got) != 1 || got[0] != keep {
		t.Errorf("another flow's entry was dropped: %+v", got)
	}
}

// A rule's stage is stamped on its findings, so flipping it must miss the cache (crosscheck 2.2): no global reset.
func TestCachedAnalyzer_StageFlipMisses(t *testing.T) {
	a := newTestAnalyzer()
	ctx := context.Background()
	doc := parseRaw(t, rawFlow, "Main")
	stable := a.Analyze(ctx, localIn(doc, AllRules(), nil, nil))
	ruleID := ""
	for _, f := range stable.Findings {
		if f.Stage == "" {
			ruleID = f.RuleID
			break
		}
	}
	if ruleID == "" {
		t.Fatal("fixture rot: no stable finding")
	}
	m, _ := LookupMeta(ruleID)
	m.Stage = StagePreview
	restore := SetRuleMetaForTesting(ruleID, m)
	preview := a.Analyze(ctx, localIn(doc, AllRules(), nil, nil))
	restore()

	if preview == stable {
		t.Fatal("a stage flip was served the report cached before it")
	}
	for _, f := range preview.Findings {
		if f.RuleID == ruleID && f.Stage != string(StagePreview) {
			t.Errorf("finding %s of the flipped rule has stage %q, want preview", f.ID, f.Stage)
		}
	}
	after := a.Analyze(ctx, localIn(doc, AllRules(), nil, nil))
	for _, f := range after.Findings {
		if f.Stage != "" {
			t.Errorf("after the restore, finding %s (%s) still has stage %q", f.ID, f.RuleID, f.Stage)
		}
	}
}

// B5: the settings digest printed options with %v, so maxDepth "2" (a string, which the rule ignores) and 2
// shared a key, and the report computed with the default threshold was served for 2.
func TestCachedAnalyzer_OptionTypesAreDistinct(t *testing.T) {
	a := newTestAnalyzer()
	doc := buildNestedLoopFlow(4, 1)
	doc.RebuildIndexes()
	withMaxDepth := func(v any) *models.AppSettings {
		return &models.AppSettings{Analysis: models.AnalysisSettings{Rules: map[string]models.RuleConfig{
			"deep-nesting": {Enabled: true, Severity: "warning", Options: map[string]interface{}{"maxDepth": v}},
		}}}
	}
	count := func(r *models.AnalysisReport) int {
		n := 0
		for _, f := range r.Findings {
			if f.RuleID == "deep-nesting" {
				n++
			}
		}
		return n
	}
	asString := a.Analyze(context.Background(), localIn(doc, AllRules(), withMaxDepth("2"), nil))
	asNumber := a.Analyze(context.Background(), localIn(doc, AllRules(), withMaxDepth(2), nil))
	if count(asString) != 0 {
		t.Fatalf("fixture rot: maxDepth \"2\" is ignored by the rule, want no finding, got %d", count(asString))
	}
	if count(asNumber) == 0 {
		t.Error("maxDepth 2 was served the report computed under maxDepth \"2\" (the default threshold)")
	}
}

// Hits and misses are counted once per request: Lookup counts its hits, the Analyze after a miss its miss.
func TestCachedAnalyzer_CountsRequests(t *testing.T) {
	a := newTestAnalyzer()
	doc := parseRaw(t, rawFlow, "Main")
	before := reportSpec.Info().Stats
	if a.Lookup(localIn(doc, AllRules(), nil, nil)) != nil {
		t.Fatal("hit on an empty cache")
	}
	a.Analyze(context.Background(), localIn(doc, AllRules(), nil, nil))
	a.Lookup(localIn(doc, AllRules(), nil, nil))
	a.Analyze(context.Background(), localIn(doc, AllRules(), nil, nil))
	after := reportSpec.Info().Stats
	if hits, misses, stores := after.Hits-before.Hits, after.Misses-before.Misses, after.Stores-before.Stores; hits != 2 || misses != 1 || stores != 1 {
		t.Errorf("hits %d, misses %d, stores %d; want 2, 1, 1", hits, misses, stores)
	}
}

// Every analyzer cache is keyed by the tenant and the rule profile it was computed under (ADR 0017).
func TestAnalyzerCacheSpecs_CarryTenantAndRuleProfile(t *testing.T) {
	found := 0
	for _, s := range cache.Specs() {
		if !strings.HasPrefix(s.Name, "analyzer.") {
			continue
		}
		found++
		has := map[cache.PartKind]bool{}
		for _, k := range s.Kinds {
			has[k] = true
		}
		for _, k := range []cache.PartKind{cache.KindTenant, cache.KindRuleProfile, cache.KindVersion, cache.KindContent} {
			if !has[k] {
				t.Errorf("%s: keys %v lack the %s part", s.Name, s.Kinds, k)
			}
		}
	}
	if found != 2 {
		t.Errorf("found %d analyzer.* caches, want 2 (reports, suppression inventory)", found)
	}
}
