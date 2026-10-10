package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"

	"pad-core/models"
)

// libFakeRule is a LibraryRule whose phases are closures. Without metadata it
// is stable, Medium and enabled (MetaOf's fallback).
type libFakeRule struct {
	id      string
	sev     models.Severity
	version int
	extract func(*RuleContext) (json.RawMessage, error)
	check   func(context.Context, Library) ([]models.LibraryFinding, error)
}

func (r *libFakeRule) ID() string          { return r.id }
func (r *libFakeRule) Name() string        { return "fake " + r.id }
func (r *libFakeRule) Description() string { return "test library rule" }
func (r *libFakeRule) DefaultSeverity() models.Severity {
	if r.sev == "" {
		return models.SeverityInfo
	}
	return r.sev
}
func (r *libFakeRule) Category() string { return "Logic" }
func (r *libFakeRule) FactsVersion() int {
	if r.version == 0 {
		return 1
	}
	return r.version
}
func (r *libFakeRule) Extract(ctx *RuleContext) (json.RawMessage, error) {
	if r.extract == nil {
		return nil, nil
	}
	return r.extract(ctx)
}
func (r *libFakeRule) CheckLibrary(ctx context.Context, lib Library) ([]models.LibraryFinding, error) {
	if r.check == nil {
		return nil, nil
	}
	return r.check(ctx, lib)
}

// libOnePerFlow reports one finding per streamed flow, on subflow "S".
func libOnePerFlow(_ context.Context, lib Library) ([]models.LibraryFinding, error) {
	var out []models.LibraryFinding
	err := lib.Each(func(s models.FlowSummary) error {
		out = append(out, models.LibraryFinding{FlowID: s.FlowID, SubflowName: "S", Finding: models.Finding{Title: "t"}})
		return nil
	})
	return out, err
}

func libSummaries(n int) []models.FlowSummary {
	out := make([]models.FlowSummary, n)
	for i := range out {
		out[i] = models.FlowSummary{
			FlowID:   fmt.Sprintf("flow-%02d", i),
			StableID: fmt.Sprintf("stable-%02d", i),
			FlowName: fmt.Sprintf("Flow %02d", i),
		}
	}
	return out
}

func libSettings(rules map[string]models.RuleConfig) *models.AppSettings {
	s := &models.AppSettings{}
	s.Analysis.Rules = rules
	return s
}

func TestLibraryRules_SeparateRegistry(t *testing.T) {
	lib := LibraryRules()
	if len(lib) == 0 {
		t.Fatal("no library rules registered")
	}
	builtin := map[string]bool{}
	for _, r := range AllRules() {
		builtin[r.ID()] = true
	}
	ids := map[string]bool{}
	for _, r := range lib {
		id := r.ID()
		if builtin[id] {
			t.Errorf("%s: a library rule must not be in AllRules() or share an ID with one", id)
		}
		if ids[id] {
			t.Errorf("%s: registered twice", id)
		}
		ids[id] = true
		m, ok := LookupMeta(id)
		if !ok {
			t.Errorf("%s: no RuleMeta", id)
			continue
		}
		if m.Stage != StageStable && m.Stage != StagePreview {
			t.Errorf("%s: Stage %q", id, m.Stage)
		}
		if m.IntroducedIn < 1 || m.IntroducedIn > RuleSetVersion || m.Revision < 1 {
			t.Errorf("%s: IntroducedIn %d / Revision %d", id, m.IntroducedIn, m.Revision)
		}
		switch m.Confidence {
		case models.ConfidenceHigh, models.ConfidenceMedium, models.ConfidenceLow:
		default:
			t.Errorf("%s: Confidence %q", id, m.Confidence)
		}
		if m.AutoFix != "" || len(m.DirectFixes) > 0 || len(m.Options) > 0 {
			t.Errorf("%s: library rules have no fixes or options (settings reach only Enabled and Severity)", id)
		}
		if !slices.Contains(knownCategories, r.Category()) {
			t.Errorf("%s: category %q is not one of %v", id, r.Category(), knownCategories)
		}
		if r.FactsVersion() < 1 {
			t.Errorf("%s: FactsVersion %d, want >= 1", id, r.FactsVersion())
		}
	}
	if !IsPreview("duplicate-subflow-across-flows") {
		t.Error("the reference library rule must be a preview")
	}
	// A custom rule under a library ID would take its settings entry and its
	// preview stamp (its per-flow findings would never gate): reserved like a built-in's.
	for id := range ids {
		cfg := CustomRuleConfig{ID: id, Severity: "error", NameMatch: "x"}
		var ce *CustomRuleError
		if cr, _, err := CompileCustomRule(cfg, CustomRuleStrict); cr != nil || !errors.As(err, &ce) || ce.Reason != ReasonReservedID {
			t.Errorf("%s: strict custom rule = %v, %v; want %s", id, cr, err, ReasonReservedID)
		}
		if cr, w, err := CompileCustomRule(cfg, CustomRuleTolerant); err != nil || cr == nil || !slices.Equal(reasons(w), []string{ReasonReservedID}) {
			t.Errorf("%s: tolerant custom rule = %v, warnings %v; want the rule with a %s warning", id, err, reasons(w), ReasonReservedID)
		}
	}
	if RuleSetVersion != 1 {
		t.Errorf("RuleSetVersion = %d: library rules do not move the built-in rule-set version", RuleSetVersion)
	}
	lib[0] = nil
	if LibraryRules()[0] == nil {
		t.Error("LibraryRules must return a copy")
	}
}

// TestLibraryRules_StayOutOfTheWalk: two identical flows produce library
// findings, but nothing of the library rule reaches the per-flow report.
func TestLibraryRules_StayOutOfTheWalk(t *testing.T) {
	a := libParse(t, "a.txt", dupTestFlow("Login", "Main"))
	b := libParse(t, "b.txt", dupTestFlow("Login", "Main"))
	rep := AnalyzeLibrary(context.Background(), []*models.FlowDocument{a, b}, LibraryRules(), models.DefaultSettings())
	if rep == nil || len(rep.Findings) == 0 {
		t.Fatalf("expected library findings, got %+v", rep)
	}
	report := RunAnalysis(a, AllRules(), models.DefaultSettings(), nil)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range LibraryRules() {
		if strings.Contains(string(data), r.ID()) {
			t.Errorf("per-flow report mentions library rule %s", r.ID())
		}
	}
}

func TestExtractFlowSummary_Basics(t *testing.T) {
	doc := libParse(t, "flows/a.txt", dupTestFlow("Login", "Main"))
	r := &libFakeRule{id: "lib-fake", version: 7, extract: func(ctx *RuleContext) (json.RawMessage, error) {
		return json.RawMessage(fmt.Sprintf(`{"subflows":%d}`, len(ctx.Flow.Subflows))), nil
	}}
	sum, err := ExtractFlowSummary(context.Background(), doc, nil, []LibraryRule{r})
	if err != nil {
		t.Fatal(err)
	}
	want := models.FlowSummary{
		FlowID:      doc.ID,
		StableID:    StableFlowID(doc),
		FlowName:    doc.Name,
		ContentHash: FlowHash(doc),
		Facts:       map[string]models.LibraryFacts{"lib-fake": {Version: 7, Data: json.RawMessage(`{"subflows":2}`)}},
	}
	if !reflect.DeepEqual(sum, want) {
		t.Errorf("summary\n got %+v\nwant %+v", sum, want)
	}
	if !strings.HasPrefix(sum.StableID, "path-") {
		t.Errorf("StableID %q: a path-backed doc must use its path identity", sum.StableID)
	}
}

func TestExtractFlowSummary_FailuresAreIsolated(t *testing.T) {
	doc := libParse(t, "a.txt", dupTestFlow("Login", "Main"))
	ok := func(*RuleContext) (json.RawMessage, error) { return json.RawMessage(`[1]`), nil }
	rules := []LibraryRule{
		&libFakeRule{id: "lib-panic", extract: func(*RuleContext) (json.RawMessage, error) { panic("boom") }},
		&libFakeRule{id: "lib-error", extract: func(*RuleContext) (json.RawMessage, error) { return nil, errors.New("no") }},
		&libFakeRule{id: "lib-big", extract: func(*RuleContext) (json.RawMessage, error) {
			return json.RawMessage(`"` + strings.Repeat("x", MaxLibraryFactsBytes) + `"`), nil
		}},
		&libFakeRule{id: "lib-invalid", extract: func(*RuleContext) (json.RawMessage, error) { return json.RawMessage(`{`), nil }},
		&libFakeRule{id: "lib-none"},
		&libFakeRule{id: "lib-ok", extract: ok},
	}
	sum, err := ExtractFlowSummary(context.Background(), doc, nil, rules)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lib-panic", "lib-error", "lib-big", "lib-invalid"}; !reflect.DeepEqual(sum.Skipped, want) {
		t.Errorf("Skipped = %v, want %v", sum.Skipped, want)
	}
	if len(sum.Facts) != 1 || string(sum.Facts["lib-ok"].Data) != `[1]` {
		t.Errorf("Facts = %v, want only lib-ok", sum.Facts)
	}
	exact := &libFakeRule{id: "lib-exact", extract: func(*RuleContext) (json.RawMessage, error) {
		return json.RawMessage(`"` + strings.Repeat("x", MaxLibraryFactsBytes-2) + `"`), nil
	}}
	if sum, _ := ExtractFlowSummary(context.Background(), doc, nil, []LibraryRule{exact}); len(sum.Skipped) != 0 {
		t.Errorf("facts of exactly MaxLibraryFactsBytes must be kept, skipped %v", sum.Skipped)
	}
}

func TestExtractFlowSummary_DisabledNilAndCancelled(t *testing.T) {
	doc := libParse(t, "a.txt", dupTestFlow("Login", "Main"))
	calls := 0
	r := &libFakeRule{id: "lib-count", extract: func(*RuleContext) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`1`), nil
	}}
	off := libSettings(map[string]models.RuleConfig{"lib-count": {Enabled: false}})
	sum, err := ExtractFlowSummary(context.Background(), doc, off, []LibraryRule{r})
	if err != nil || calls != 0 || sum.Facts != nil {
		t.Errorf("disabled rule: err=%v calls=%d facts=%v", err, calls, sum.Facts)
	}
	if _, err := ExtractFlowSummary(context.Background(), nil, nil, []LibraryRule{r}); err == nil {
		t.Error("nil doc: want an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExtractFlowSummary(ctx, doc, nil, []LibraryRule{r}); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Errorf("cancelled: err=%v calls=%d", err, calls)
	}
}

func TestRunLibraryRules_StampsAndSorts(t *testing.T) {
	sums := libSummaries(3)
	sums[0].FlowName, sums[1].FlowName, sums[2].FlowName = "B", "A", "A"
	// StableIDs of the two "A" against FlowID order: the tie-break must be the
	// StableID (FlowID is a fresh doc.ID on every desktop parse).
	sums[1].StableID, sums[2].StableID = "stable-02", "stable-01"
	stableOf := map[string]string{}
	for _, s := range sums {
		stableOf[s.FlowID] = s.StableID
	}
	r := &libFakeRule{id: "lib-stamp", sev: models.SeverityWarning, check: func(_ context.Context, lib Library) ([]models.LibraryFinding, error) {
		var out []models.LibraryFinding
		err := lib.Each(func(s models.FlowSummary) error {
			out = append(out,
				models.LibraryFinding{FlowID: s.FlowID, SubflowName: "Z", Finding: models.Finding{RuleID: "spoofed", Stage: "preview"}},
				models.LibraryFinding{FlowID: s.FlowID, SubflowName: "Y", Finding: models.Finding{Severity: models.SeverityError}})
			return nil
		})
		slices.Reverse(out)
		return out, err
	}}
	rep := RunLibraryRules(context.Background(), []LibraryRule{r}, NewMemoryLibrary(sums), nil)
	if rep.Flows != 3 || rep.Truncated || rep.RulesRun != 1 || rep.RulesSkipped != 0 || len(rep.Findings) != 6 {
		t.Fatalf("report %+v", rep)
	}
	// FlowName, then StableID (flow-02's stable-01 first for the two "A"), then subflow.
	wantOrder := []string{"flow-02/Y", "flow-02/Z", "flow-01/Y", "flow-01/Z", "flow-00/Y", "flow-00/Z"}
	for i, f := range rep.Findings {
		if got := f.FlowID + "/" + f.SubflowName; got != wantOrder[i] {
			t.Errorf("finding %d is %s, want %s", i, got, wantOrder[i])
		}
		if f.ID != fmt.Sprintf("L-%03d", i+1) {
			t.Errorf("finding %d ID %q", i, f.ID)
		}
		if f.RuleID != "lib-stamp" || f.Category != "Logic" || f.Confidence != models.ConfidenceMedium || f.Stage != "" {
			t.Errorf("finding %d not stamped: %+v", i, f.Finding)
		}
		wantSev := models.SeverityWarning
		if f.SubflowName == "Y" {
			wantSev = models.SeverityError // rule-graded, kept
		}
		if f.Severity != wantSev {
			t.Errorf("finding %d severity %s, want %s", i, f.Severity, wantSev)
		}
		if want := libraryFingerprint("lib-stamp", stableOf[f.FlowID], f.SubflowName, ""); f.Fingerprint != want {
			t.Errorf("finding %d fingerprint %s, want %s", i, f.Fingerprint, want)
		}
		if f.FlowName == "" {
			t.Errorf("finding %d: FlowName not filled from the summary", i)
		}
	}
}

func TestRunLibraryRules_SeverityOverrideD13(t *testing.T) {
	r := &libFakeRule{id: "lib-sev", sev: models.SeverityInfo, check: func(_ context.Context, lib Library) ([]models.LibraryFinding, error) {
		return []models.LibraryFinding{
			{FlowID: "flow-00", SubflowName: "graded", Finding: models.Finding{Severity: models.SeverityWarning}},
			{FlowID: "flow-00", SubflowName: "plain"},
		}, lib.Each(func(models.FlowSummary) error { return nil })
	}}
	sev := func(s *models.AppSettings) map[string]models.Severity {
		rep := RunLibraryRules(context.Background(), []LibraryRule{r}, NewMemoryLibrary(libSummaries(1)), s)
		out := map[string]models.Severity{}
		for _, f := range rep.Findings {
			out[f.SubflowName] = f.Severity
		}
		return out
	}
	cases := []struct {
		name     string
		settings *models.AppSettings
		want     map[string]models.Severity
	}{
		{"nil settings", nil, map[string]models.Severity{"graded": "warning", "plain": "info"}},
		{"configured default", libSettings(map[string]models.RuleConfig{"lib-sev": {Enabled: true, Severity: "info"}}),
			map[string]models.Severity{"graded": "warning", "plain": "info"}},
		{"configured other", libSettings(map[string]models.RuleConfig{"lib-sev": {Enabled: true, Severity: "error"}}),
			map[string]models.Severity{"graded": "error", "plain": "error"}},
	}
	for _, c := range cases {
		if got := sev(c.settings); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRunLibraryRules_EnabledAndStage(t *testing.T) {
	r := &libFakeRule{id: "lib-meta", check: libOnePerFlow}
	lib := NewMemoryLibrary(libSummaries(2))
	off := libSettings(map[string]models.RuleConfig{"lib-meta": {Enabled: false}})
	if rep := RunLibraryRules(context.Background(), []LibraryRule{r}, lib, off); rep.RulesRun != 0 || len(rep.Findings) != 0 {
		t.Errorf("disabled: %+v", rep)
	}

	restore := SetRuleMetaForTesting("lib-meta", RuleMeta{Confidence: models.ConfidenceLow, Stage: StagePreview, IntroducedIn: 1, Revision: 1})
	rep := RunLibraryRules(context.Background(), []LibraryRule{r}, lib, nil)
	restore()
	if len(rep.Findings) != 2 {
		t.Fatalf("preview: %+v", rep)
	}
	for _, f := range rep.Findings {
		if f.Stage != string(StagePreview) || f.Confidence != models.ConfidenceLow {
			t.Errorf("preview stamp: %+v", f.Finding)
		}
	}

	restore = SetRuleMetaForTesting("lib-meta", RuleMeta{Confidence: models.ConfidenceMedium, Stage: StageStable, DisabledByDefault: true})
	defer restore()
	if rep := RunLibraryRules(context.Background(), []LibraryRule{r}, lib, &models.AppSettings{}); rep.RulesRun != 0 {
		t.Errorf("DisabledByDefault without an entry ran: %+v", rep)
	}
	on := libSettings(map[string]models.RuleConfig{"lib-meta": {Enabled: true}})
	if rep := RunLibraryRules(context.Background(), []LibraryRule{r}, lib, on); rep.RulesRun != 1 || len(rep.Findings) != 2 {
		t.Errorf("DisabledByDefault enabled in settings: %+v", rep)
	}
}

func TestRunLibraryRules_PanicsAndErrorsAreIsolated(t *testing.T) {
	rules := []LibraryRule{
		&libFakeRule{id: "lib-panic", check: func(context.Context, Library) ([]models.LibraryFinding, error) { panic("boom") }},
		&libFakeRule{id: "lib-error", check: func(ctx context.Context, lib Library) ([]models.LibraryFinding, error) {
			found, _ := libOnePerFlow(ctx, lib)
			return found, errors.New("no")
		}},
		&libFakeRule{id: "lib-ok", check: libOnePerFlow},
	}
	rep := RunLibraryRules(context.Background(), rules, NewMemoryLibrary(libSummaries(2)), nil)
	if rep.RulesRun != 3 || rep.RulesSkipped != 2 || len(rep.Findings) != 2 {
		t.Fatalf("report %+v", rep)
	}
	for _, f := range rep.Findings {
		if f.RuleID != "lib-ok" {
			t.Errorf("finding of a failed rule kept: %+v", f)
		}
	}
}

func TestRunLibraryRules_CancelledBetweenRules(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := false
	rules := []LibraryRule{
		&libFakeRule{id: "lib-cancel", check: func(ctx context.Context, lib Library) ([]models.LibraryFinding, error) {
			found, err := libOnePerFlow(ctx, lib)
			cancel()
			return found, err
		}},
		&libFakeRule{id: "lib-after", check: func(ctx context.Context, lib Library) ([]models.LibraryFinding, error) {
			second = true
			return libOnePerFlow(ctx, lib)
		}},
	}
	rep := RunLibraryRules(ctx, rules, NewMemoryLibrary(libSummaries(2)), nil)
	if second || rep.RulesRun != 2 || rep.RulesSkipped != 2 || len(rep.Findings) != 0 {
		t.Errorf("second ran=%v report %+v", second, rep)
	}
}

func TestRunLibraryRules_FlowCap(t *testing.T) {
	var seen []string
	var length int
	r := &libFakeRule{id: "lib-cap", check: func(_ context.Context, lib Library) ([]models.LibraryFinding, error) {
		length = lib.Len()
		err := lib.Each(func(s models.FlowSummary) error {
			seen = append(seen, s.FlowID)
			return nil
		})
		// flow-04 is beyond the cap: a finding naming it is dropped.
		return []models.LibraryFinding{{FlowID: "flow-00"}, {FlowID: "flow-04"}, {FlowID: "nowhere"}}, err
	}}
	sums := libSummaries(5)
	slices.Reverse(sums)
	rep := runLibraryRules(context.Background(), []LibraryRule{r}, NewMemoryLibrary(sums), nil, 3)
	if !reflect.DeepEqual(seen, []string{"flow-00", "flow-01", "flow-02"}) || length != 3 {
		t.Errorf("rule saw %v (Len %d), want the first three by FlowID", seen, length)
	}
	if rep.Flows != 3 || !rep.Truncated || len(rep.Findings) != 1 || rep.Findings[0].FlowID != "flow-00" {
		t.Errorf("report %+v", rep)
	}
	if rep := runLibraryRules(context.Background(), []LibraryRule{r}, NewMemoryLibrary(libSummaries(3)), nil, 3); rep.Truncated || rep.Flows != 3 {
		t.Errorf("exactly at the cap: %+v", rep)
	}
}

func TestRunLibraryRules_FactsIsolationAndVersions(t *testing.T) {
	sums := libSummaries(1)
	sums[0].Facts = map[string]models.LibraryFacts{
		"lib-a": {Version: 1, Data: json.RawMessage(`"a"`)},
		"lib-b": {Version: 2, Data: json.RawMessage(`"b"`)},
	}
	got := map[string]map[string]models.LibraryFacts{}
	record := func(id string) func(context.Context, Library) ([]models.LibraryFinding, error) {
		return func(_ context.Context, lib Library) ([]models.LibraryFinding, error) {
			return nil, lib.Each(func(s models.FlowSummary) error {
				got[id] = s.Facts
				return nil
			})
		}
	}
	rules := []LibraryRule{
		&libFakeRule{id: "lib-a", check: record("lib-a")},
		&libFakeRule{id: "lib-b", version: 3, check: record("lib-b")},
	}
	RunLibraryRules(context.Background(), rules, NewMemoryLibrary(sums), nil)
	if want := map[string]models.LibraryFacts{"lib-a": sums[0].Facts["lib-a"]}; !reflect.DeepEqual(got["lib-a"], want) {
		t.Errorf("lib-a saw %v, want only its own facts", got["lib-a"])
	}
	if got["lib-b"] != nil {
		t.Errorf("lib-b saw %v, want no stale (version 2 != 3) facts", got["lib-b"])
	}
	if len(sums[0].Facts) != 2 {
		t.Error("the caller's summary was modified")
	}
}

func TestRunLibraryRules_DedupAndCaps(t *testing.T) {
	var related []models.LibraryLocation
	for i := range maxLibraryRelated + 5 {
		related = append(related, models.LibraryLocation{FlowID: fmt.Sprintf("f%02d", i), FlowName: fmt.Sprintf("N%02d", 30-i)})
	}
	r := &libFakeRule{id: "lib-dedup", check: func(_ context.Context, lib Library) ([]models.LibraryFinding, error) {
		return []models.LibraryFinding{
			{FlowID: "flow-00", SubflowName: "S", Finding: models.Finding{Title: "first"}, Related: related},
			{FlowID: "flow-00", SubflowName: "S", Finding: models.Finding{Title: "second"}},
			{FlowID: "flow-00", SubflowName: "S", Anchor: "#1", Finding: models.Finding{Title: "anchored"}},
		}, lib.Each(func(models.FlowSummary) error { return nil })
	}}
	rep := RunLibraryRules(context.Background(), []LibraryRule{r}, NewMemoryLibrary(libSummaries(1)), nil)
	if len(rep.Findings) != 2 || rep.Truncated {
		t.Fatalf("report %+v", rep)
	}
	f := rep.Findings[0]
	if len(f.Related) != maxLibraryRelated {
		t.Errorf("related %d, want the cap %d", len(f.Related), maxLibraryRelated)
	}
	if !slices.IsSortedFunc(f.Related, func(a, b models.LibraryLocation) int { return strings.Compare(a.FlowName, b.FlowName) }) ||
		f.Related[0].FlowName != "N06" {
		t.Errorf("related not sorted by flow name before the cap: %v", f.Related[:3])
	}
	if related[0].FlowName != "N30" {
		t.Error("the rule's Related slice was sorted in place")
	}
	if rep.Findings[0].Fingerprint == rep.Findings[1].Fingerprint {
		t.Error("the anchor must separate fingerprints")
	}

	many := &libFakeRule{id: "lib-many", check: func(_ context.Context, lib Library) ([]models.LibraryFinding, error) {
		out := make([]models.LibraryFinding, maxLibraryFindingsPerRule+1)
		for i := range out {
			out[i] = models.LibraryFinding{FlowID: "flow-00", SubflowName: fmt.Sprintf("S%05d", i)}
		}
		return out, lib.Each(func(models.FlowSummary) error { return nil })
	}}
	rep = RunLibraryRules(context.Background(), []LibraryRule{many}, NewMemoryLibrary(libSummaries(1)), nil)
	if len(rep.Findings) != maxLibraryFindingsPerRule || !rep.Truncated {
		t.Errorf("findings cap: %d findings, truncated %v", len(rep.Findings), rep.Truncated)
	}
}

func TestRunLibraryRules_DeterministicOverInputOrder(t *testing.T) {
	var docs []*models.FlowDocument
	for i := range 12 {
		name := fmt.Sprintf("flows/f%02d.txt", i)
		docs = append(docs, libParse(t, name, dupTestFlow(fmt.Sprintf("Login%d", i%3), fmt.Sprintf("Main%d", i%4))))
	}
	var sums []models.FlowSummary
	for _, d := range docs {
		s, err := ExtractFlowSummary(context.Background(), d, nil, LibraryRules())
		if err != nil {
			t.Fatal(err)
		}
		sums = append(sums, s)
	}
	render := func(s []models.FlowSummary) string {
		rep := RunLibraryRules(context.Background(), LibraryRules(), NewMemoryLibrary(s), nil)
		data, err := json.Marshal(rep.Findings)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	want := render(sums)
	if !strings.Contains(want, "duplicate-subflow-across-flows") {
		t.Fatalf("expected findings, got %s", want)
	}
	rng := rand.New(rand.NewSource(1))
	for range 5 {
		shuffled := slices.Clone(sums)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := render(shuffled); got != want {
			t.Fatalf("output depends on input order:\n got %s\nwant %s", got, want)
		}
	}
}

// TestLibraryFingerprint_Pinned: library fingerprints are stored data.
func TestLibraryFingerprint_Pinned(t *testing.T) {
	cases := []struct{ rule, stable, subflow, anchor, want string }{
		{"duplicate-subflow-across-flows", "path-0123456789abcdef", "Login", "", "duplicate-subflow-across-flows:a6c63f49cb4223f5"},
		{"duplicate-subflow-across-flows", "path-0123456789abcdef", "Login", "#1", "duplicate-subflow-across-flows:49160519fb400bad"},
		{"duplicate-subflow-across-flows", "6f9c2a5e-flow", "", "", "duplicate-subflow-across-flows:7908594846564ce0"},
	}
	for _, c := range cases {
		if got := libraryFingerprint(c.rule, c.stable, c.subflow, c.anchor); got != c.want {
			t.Errorf("libraryFingerprint(%q, %q, %q, %q) = %s, want %s", c.rule, c.stable, c.subflow, c.anchor, got, c.want)
		}
	}
	if libraryFingerprint("r", "ab", "c", "") == libraryFingerprint("r", "a", "bc", "") {
		t.Error("parts must be separated")
	}
}

func TestNewMemoryLibrary_OrderAndStop(t *testing.T) {
	in := []models.FlowSummary{{FlowID: "b"}, {FlowID: "a", StableID: "2"}, {FlowID: "a", StableID: "1"}}
	lib := NewMemoryLibrary(in)
	var got []string
	stop := errors.New("stop")
	err := lib.Each(func(s models.FlowSummary) error {
		got = append(got, s.FlowID+s.StableID)
		if len(got) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || !reflect.DeepEqual(got, []string{"a1", "a2"}) || lib.Len() != 3 {
		t.Errorf("Each = %v, %v (Len %d)", got, err, lib.Len())
	}
	if in[0].FlowID != "b" {
		t.Error("NewMemoryLibrary sorted the caller's slice")
	}
	if rep := RunLibraryRules(context.Background(), LibraryRules(), nil, nil); rep.Flows != 0 || rep.Findings == nil {
		t.Errorf("nil library: %+v", rep)
	}
}

func TestAnalyzeLibrary_NilWhenNoRuleEnabled(t *testing.T) {
	off := map[string]models.RuleConfig{}
	for _, r := range LibraryRules() {
		off[r.ID()] = models.RuleConfig{Enabled: false}
	}
	docs := []*models.FlowDocument{libParse(t, "a.txt", dupTestFlow("Login", "Main"))}
	if rep := AnalyzeLibrary(context.Background(), docs, LibraryRules(), libSettings(off)); rep != nil {
		t.Errorf("all library rules disabled: %+v, want nil", rep)
	}
	if rep := AnalyzeLibrary(context.Background(), nil, LibraryRules(), nil); rep == nil || rep.Flows != 0 {
		t.Errorf("no docs: %+v", rep)
	}
}

// TestAnalyzeLibrary_DropsFailedExtractions: a doc whose extraction fails (a
// nil doc here) is left out, not passed to the rules as an empty summary.
func TestAnalyzeLibrary_DropsFailedExtractions(t *testing.T) {
	a := libParse(t, "a.txt", dupTestFlow("Login", "Main"))
	b := libParse(t, "b.txt", dupTestFlow("Login", "Main"))
	var seen []string
	probe := &libFakeRule{id: "lib-probe", check: func(_ context.Context, lib Library) ([]models.LibraryFinding, error) {
		return nil, lib.Each(func(s models.FlowSummary) error {
			seen = append(seen, s.FlowID)
			return nil
		})
	}}
	rules := append(LibraryRules(), probe)
	rep := AnalyzeLibrary(context.Background(), []*models.FlowDocument{a, nil, b}, rules, nil)
	if rep == nil || rep.Flows != 2 || rep.RulesSkipped != 0 || len(rep.Findings) == 0 {
		t.Fatalf("report %+v, want 2 flows and findings", rep)
	}
	if want := []string{min(a.ID, b.ID), max(a.ID, b.ID)}; !slices.Equal(seen, want) {
		t.Errorf("rules saw flows %q, want %q", seen, want)
	}
	for _, f := range rep.Findings {
		if f.FlowID == "" {
			t.Errorf("finding without a flow: %+v", f)
		}
	}
}

// TestAnalyzeLibrary_MatchesSequential: the parallel extraction gives the same
// report as extracting one doc at a time (run with -race).
func TestAnalyzeLibrary_MatchesSequential(t *testing.T) {
	var docs []*models.FlowDocument
	for i := range 40 {
		docs = append(docs, libParse(t, fmt.Sprintf("flows/f%02d.txt", i), dupTestFlow(fmt.Sprintf("Login%d", i%7), fmt.Sprintf("Main%d", i%5))))
	}
	var sums []models.FlowSummary
	for _, d := range docs {
		s, err := ExtractFlowSummary(context.Background(), d, nil, LibraryRules())
		if err != nil {
			t.Fatal(err)
		}
		sums = append(sums, s)
	}
	want := RunLibraryRules(context.Background(), LibraryRules(), NewMemoryLibrary(sums), nil)
	got := AnalyzeLibrary(context.Background(), docs, LibraryRules(), nil)
	if len(want.Findings) == 0 || !reflect.DeepEqual(got.Findings, want.Findings) || got.Flows != want.Flows {
		t.Errorf("parallel %d findings, sequential %d", len(got.Findings), len(want.Findings))
	}
}
