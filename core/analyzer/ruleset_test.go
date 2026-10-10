package analyzer

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"pad-core/models"
)

// TestRuleSetDigest_Pinned pins the persisted digest encoding. The values were
// derived independently (Python hashlib over the documented encoding: refs
// sorted by (ID, Rev, Stage), duplicates dropped, each field as
// "<byte length>:<bytes>"). A change here invalidates every stored baseline
// and snapshot rule set: it needs a new prefix instead.
func TestRuleSetDigest_Pinned(t *testing.T) {
	cases := []struct {
		name string
		refs []models.RuleRef
		want string
	}{
		{"empty", nil, "rs1:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"one built-in", []models.RuleRef{{ID: "wait-zero"}}, "rs1:f5e57dde13710f3ba0ae712d146bc2b0401358d992c031311d5ee5d5e5108b07"},
		{"the same as preview", []models.RuleRef{{ID: "wait-zero", Stage: "preview"}}, "rs1:05b0b86ed86bbc12d1dd10e499f226784bc0b4353f67969c0c883eda686ac6d4"},
		{"mixed, unsorted, duplicated", []models.RuleRef{
			{ID: "x-preview", Stage: "preview"},
			{ID: "wait-zero"},
			{ID: "org-no-msgbox", Rev: "9b7bed4fc516fa2a"},
			{ID: "circular-subflow-dependency", Rev: "r2"},
			{ID: "wait-zero"},
		}, "rs1:2a95345053f589f3a842e5a8d44b05d4c71b374ed8e454b85e76d72bb15fec79"},
		// Length prefixes keep field boundaries: these two must differ.
		{"a|bc", []models.RuleRef{{ID: "a", Rev: "bc"}}, "rs1:8dce64c00e38a60883eba490464eab934171da7e12d47498fd99c5239f9663be"},
		{"ab|c", []models.RuleRef{{ID: "ab", Rev: "c"}}, "rs1:118a87ef2d4b39ec268bd69e227912254f6d99d1f13f864c6bb973799a8b10e6"},
	}
	for _, c := range cases {
		if got := NewRuleSet(c.refs).Digest; got != c.want {
			t.Errorf("%s: digest %s, want %s", c.name, got, c.want)
		}
	}
}

func TestNewRuleSet_SortedDistinctAndInputUntouched(t *testing.T) {
	in := []models.RuleRef{{ID: "b"}, {ID: "a", Rev: "r2"}, {ID: "b"}, {ID: "a"}, {ID: "a", Stage: "preview"}}
	orig := slices.Clone(in)
	rs := NewRuleSet(in)
	want := []models.RuleRef{{ID: "a"}, {ID: "a", Stage: "preview"}, {ID: "a", Rev: "r2"}, {ID: "b"}}
	if !slices.Equal(rs.Rules, want) {
		t.Errorf("Rules = %v, want %v", rs.Rules, want)
	}
	if !slices.Equal(in, orig) {
		t.Errorf("NewRuleSet modified its input: %v", in)
	}
	rev := slices.Clone(orig)
	slices.Reverse(rev)
	if NewRuleSet(rev).Digest != rs.Digest {
		t.Error("the digest must not depend on input order")
	}

	empty := NewRuleSet(nil)
	b, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), `{"rules":[],"digest":"rs1:`) {
		t.Errorf("an empty set must marshal as rules:[]: %s", b)
	}
}

func TestRuleRefOf(t *testing.T) {
	if got := RuleRefOf(&WaitZeroRule{}); got != (models.RuleRef{ID: "wait-zero"}) {
		t.Errorf("wait-zero (Revision 1): %+v", got)
	}
	if got := RuleRefOf(&CircularSubflowRule{}); got != (models.RuleRef{ID: "circular-subflow-dependency", Rev: "r2"}) {
		t.Errorf("circular-subflow-dependency (Revision 2): %+v", got)
	}

	m := previewMetaOf(t, "wait-zero")
	m.Revision = 3
	restore := SetRuleMetaForTesting("wait-zero", m)
	if got := RuleRefOf(&WaitZeroRule{}); got != (models.RuleRef{ID: "wait-zero", Rev: "r3", Stage: "preview"}) {
		t.Errorf("wait-zero as a preview at Revision 3: %+v", got)
	}
	restore()

	cr, err := NewCustomRule(CustomRuleConfig{ID: "org-no-msgbox", Name: "No message boxes", Severity: "warning", RawTypeMatch: "Display.ShowMessageBox"})
	if err != nil {
		t.Fatal(err)
	}
	if got := RuleRefOf(cr); got != (models.RuleRef{ID: "org-no-msgbox", Rev: cr.MatchDigest()}) || got.Rev == "" {
		t.Errorf("custom rule: %+v (MatchDigest %q)", got, cr.MatchDigest())
	}
	if got := RuleRefOf(rulesetFakeRule{id: "fake"}); got != (models.RuleRef{ID: "fake"}) {
		t.Errorf("unregistered rule: %+v", got)
	}
}

// rulesetFakeRule is an unregistered, non-custom rule.
type rulesetFakeRule struct{ id string }

func (r rulesetFakeRule) ID() string                                       { return r.id }
func (rulesetFakeRule) Name() string                                       { return "fake" }
func (rulesetFakeRule) Description() string                                { return "" }
func (rulesetFakeRule) DefaultSeverity() models.Severity                   { return models.SeverityWarning }
func (rulesetFakeRule) Category() string                                   { return "" }
func (rulesetFakeRule) Check(*models.Block, *RuleContext) []models.Finding { return nil }

func TestRecordRuleSet_AllRules(t *testing.T) {
	all := AllRules()
	rs := RecordRuleSet(all)
	if len(rs.Rules) != len(all) {
		t.Fatalf("%d refs for %d rules", len(rs.Rules), len(all))
	}
	for _, r := range all {
		if !slices.Contains(rs.Rules, RuleRefOf(r)) {
			t.Errorf("%s missing from the record", r.ID())
		}
	}
	shuffled := slices.Clone(all)
	slices.Reverse(shuffled)
	shuffled = append(shuffled, all[0])
	if got := RecordRuleSet(shuffled); got.Digest != rs.Digest {
		t.Error("the record must not depend on rule order or repeats")
	}
	if !strings.HasPrefix(rs.Digest, "rs1:") || len(rs.Digest) != len("rs1:")+64 {
		t.Errorf("digest shape: %q", rs.Digest)
	}
}

// TestRunAnalysis_StampsRuleSet: the report records the rules that ran, i.e.
// the enabled ones, so a rule disabled by settings is not part of it.
func TestRunAnalysis_StampsRuleSet(t *testing.T) {
	doc := parseGoldenText(t, goldenSuppressionsFlow, "Main.txt")
	settings := models.DefaultSettings()
	rc := settings.Analysis.Rules["wait-zero"]
	rc.Enabled = false
	settings.Analysis.Rules["wait-zero"] = rc

	report := RunAnalysis(doc, AllRules(), settings, nil)
	if report.RuleSet == nil {
		t.Fatal("RuleSet not stamped")
	}
	var enabled []Rule
	for _, r := range AllRules() {
		if r.ID() != "wait-zero" {
			enabled = append(enabled, r)
		}
	}
	if want := RecordRuleSet(enabled); report.RuleSet.Digest != want.Digest || !slices.Equal(report.RuleSet.Rules, want.Rules) {
		t.Errorf("RuleSet %s, want the enabled rules' %s", report.RuleSet.Digest, want.Digest)
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), report.RuleSet.Digest) {
		t.Error("RuleSet must not be serialized with the report")
	}
}

func TestKnownToLegacyRecords(t *testing.T) {
	for _, r := range AllRules() {
		m := MetaOf(r)
		want := m.IntroducedIn <= 1 && m.Revision <= 1
		if got := KnownToLegacyRecords(RuleRefOf(r)); got != want {
			t.Errorf("%s (IntroducedIn %d, Revision %d): known %t, want %t", r.ID(), m.IntroducedIn, m.Revision, got, want)
		}
	}
	if KnownToLegacyRecords(models.RuleRef{ID: "circular-subflow-dependency", Rev: "r2"}) {
		t.Error("a D7-revised rule must be new to legacy records")
	}
	if !KnownToLegacyRecords(models.RuleRef{ID: "org-no-msgbox", Rev: "9b7bed4fc516fa2a"}) {
		t.Error("custom rules are known to legacy records")
	}
	if !KnownToLegacyRecords(models.RuleRef{ID: "circular-subflow-dependency", Rev: "9b7bed4fc516fa2a"}) {
		t.Error("a custom rule reusing a built-in's ID is still a custom rule")
	}
	if !KnownToLegacyRecords(models.RuleRef{ID: "fake"}) {
		t.Error("a rule that is not a built-in is known (its findings stay drift)")
	}

	m, _ := LookupMeta("wait-zero")
	m.IntroducedIn = 2
	t.Cleanup(SetRuleMetaForTesting("wait-zero", m))
	if KnownToLegacyRecords(models.RuleRef{ID: "wait-zero"}) {
		t.Error("a rule of rule-set version 2 is new to legacy records")
	}
}

func TestBuiltinRev(t *testing.T) {
	for rev, want := range map[string]bool{
		"": true, "r2": true, "r10": true,
		"r": false, "r2a": false, "R2": false, "2": false, "9b7bed4fc516fa2a": false, "rr": false,
	} {
		if got := builtinRev(rev); got != want {
			t.Errorf("builtinRev(%q) = %t, want %t", rev, got, want)
		}
	}
}

func rulesetFinding(ruleID string) *models.Finding { return &models.Finding{RuleID: ruleID} }

func TestRuleAge_Recorded(t *testing.T) {
	cr, err := NewCustomRule(CustomRuleConfig{ID: "org-no-msgbox", Severity: "warning", RawTypeMatch: "Display.ShowMessageBox"})
	if err != nil {
		t.Fatal(err)
	}
	baseline := RecordRuleSet([]Rule{&WaitZeroRule{}, &DeadCodeRule{}, cr})

	edited, err := NewCustomRule(CustomRuleConfig{ID: "org-no-msgbox", Severity: "warning", RawTypeMatch: "Display.ShowMessage"})
	if err != nil {
		t.Fatal(err)
	}
	cosmetic, err := NewCustomRule(CustomRuleConfig{ID: "org-no-msgbox", Name: "Renamed", Description: "x", Severity: "error", RawTypeMatch: "Display.ShowMessageBox"})
	if err != nil {
		t.Fatal(err)
	}

	same := NewRuleAge(baseline, RecordRuleSet([]Rule{&DeadCodeRule{}, cr, &WaitZeroRule{}}))
	for _, id := range []string{"wait-zero", "dead-code", "org-no-msgbox", "unknown"} {
		if same.IsNew(rulesetFinding(id)) {
			t.Errorf("same set: %s must not be new", id)
		}
	}

	added := NewRuleAge(baseline, RecordRuleSet([]Rule{&WaitZeroRule{}, &DeadCodeRule{}, cr, &EmptyBranchRule{}}))
	if !added.IsNew(rulesetFinding("empty-branch")) || added.IsNew(rulesetFinding("dead-code")) {
		t.Error("an added rule (an org toggle) is new, the others are not")
	}

	if !NewRuleAge(baseline, RecordRuleSet([]Rule{&WaitZeroRule{}, &DeadCodeRule{}, edited})).IsNew(rulesetFinding("org-no-msgbox")) {
		t.Error("a custom rule whose matcher changed is new")
	}
	if NewRuleAge(baseline, RecordRuleSet([]Rule{&WaitZeroRule{}, &DeadCodeRule{}, cosmetic})).IsNew(rulesetFinding("org-no-msgbox")) {
		t.Error("a custom rule whose name, description or severity changed is not new")
	}

	// Promotion: the baseline saw wait-zero as a preview, it is stable now.
	restore := SetRuleMetaForTesting("wait-zero", previewMetaOf(t, "wait-zero"))
	previewBaseline := RecordRuleSet([]Rule{&WaitZeroRule{}, &DeadCodeRule{}})
	restore()
	promoted := NewRuleAge(previewBaseline, RecordRuleSet([]Rule{&WaitZeroRule{}, &DeadCodeRule{}}))
	if !promoted.IsNew(rulesetFinding("wait-zero")) || promoted.IsNew(rulesetFinding("dead-code")) {
		t.Error("a promoted rule is new")
	}

	// Revision bump.
	m, _ := LookupMeta("dead-code")
	m.Revision = 2
	restore = SetRuleMetaForTesting("dead-code", m)
	revised := NewRuleAge(baseline, RecordRuleSet([]Rule{&WaitZeroRule{}, &DeadCodeRule{}, cr}))
	restore()
	if !revised.IsNew(rulesetFinding("dead-code")) || revised.IsNew(rulesetFinding("wait-zero")) {
		t.Error("a revised rule is new")
	}

	if (RuleAge{}).IsNew(rulesetFinding("dead-code")) || NewRuleAge(baseline, nil).IsNew(rulesetFinding("empty-branch")) {
		t.Error("the zero RuleAge and a nil current set know nothing new")
	}
}

func TestRuleAge_Legacy(t *testing.T) {
	current := RecordRuleSet(AllRules())
	other := NewRuleSet([]models.RuleRef{{ID: "dead-code"}})
	for _, recorded := range []*models.RuleSet{
		nil,
		{Rules: []models.RuleRef{{ID: "wait-zero"}}, Digest: "rs2:future"},
		{Rules: []models.RuleRef{{ID: "wait-zero"}}},
		// Digest-only (a storage read without the rules, a JSON null) and
		// rules that do not hash to the digest: not a usable record.
		{Digest: other.Digest},
		{Rules: []models.RuleRef{{ID: "wait-zero"}}, Digest: other.Digest},
	} {
		age := NewRuleAge(recorded, current)
		for _, r := range AllRules() {
			if got, want := age.IsNew(rulesetFinding(r.ID())), !KnownToLegacyRecords(RuleRefOf(r)); got != want {
				t.Errorf("recorded %+v: %s new %t, want %t", recorded, r.ID(), got, want)
			}
		}
		if !age.IsNew(rulesetFinding("circular-subflow-dependency")) || age.IsNew(rulesetFinding("wait-zero")) {
			t.Errorf("recorded %+v: D7 rules are new to a legacy record, version-1 rules are not", recorded)
		}
	}
}

// TestRuleAge_SameDigest: a record with the current digest saw the current
// set, whatever its Rules hold (a digest-only read included).
func TestRuleAge_SameDigest(t *testing.T) {
	current := RecordRuleSet(AllRules())
	for _, recorded := range []*models.RuleSet{
		{Digest: current.Digest},
		{Rules: []models.RuleRef{}, Digest: current.Digest},
		{Rules: []models.RuleRef{{ID: "wait-zero"}}, Digest: current.Digest},
		NewRuleSet(current.Rules),
	} {
		age := NewRuleAge(recorded, current)
		var n int
		for _, r := range AllRules() {
			if age.IsNew(rulesetFinding(r.ID())) {
				n++
			}
		}
		if n != 0 {
			t.Errorf("recorded %d rules with the current digest: %d of %d rules new, want 0", len(recorded.Rules), n, len(AllRules()))
		}
	}
}

// TestRuleAge_SharedID: a custom rule reusing a built-in's ID gives two refs
// under one ID; the finding is new only when both are, so it fails toward
// drift.
func TestRuleAge_SharedID(t *testing.T) {
	cr, _, err := CompileCustomRule(CustomRuleConfig{ID: "circular-subflow-dependency", Severity: "warning", NameMatch: "x"}, CustomRuleTolerant)
	if err != nil {
		t.Fatal(err)
	}
	current := RecordRuleSet([]Rule{&CircularSubflowRule{}, cr})
	if len(current.Rules) != 2 {
		t.Fatalf("want two refs under one ID, got %v", current.Rules)
	}
	if NewRuleAge(nil, current).IsNew(rulesetFinding("circular-subflow-dependency")) {
		t.Error("legacy: the custom ref is known, so the shared ID is not new")
	}
	if !NewRuleAge(RecordRuleSet(nil), current).IsNew(rulesetFinding("circular-subflow-dependency")) {
		t.Error("an empty record saw neither ref, so the shared ID is new")
	}
	if NewRuleAge(RecordRuleSet([]Rule{cr}), current).IsNew(rulesetFinding("circular-subflow-dependency")) {
		t.Error("the record saw one of the two refs: not new")
	}
}

func TestSplitDrift(t *testing.T) {
	withPreviewRule(t, "wait-zero")
	baseline := RecordRuleSet([]Rule{&DeadCodeRule{}})
	current := RecordRuleSet([]Rule{&DeadCodeRule{}, &EmptyBranchRule{}, &WaitZeroRule{}, &UnusedVariableRule{}})
	age := NewRuleAge(baseline, current)
	fs := []models.Finding{
		{ID: "F-1", RuleID: "dead-code", Severity: models.SeverityError},
		{ID: "F-2", RuleID: "unused-variable", Severity: models.SeverityInfo},
		{ID: "F-3", RuleID: "wait-zero", Severity: models.SeverityError},
		{ID: "F-4", RuleID: "empty-branch", Severity: models.SeverityWarning},
		{ID: "F-5", RuleID: "dead-code", Severity: models.SeverityWarning, Stage: "preview"},
		{ID: "F-6", RuleID: "parse-error", Severity: models.SeverityError},
		{ID: "F-7", RuleID: "unused-variable", Severity: models.SeverityInfo},
	}
	s := SplitDrift(fs, age)
	ids := func(fs []models.Finding) (out []string) {
		for _, f := range fs {
			out = append(out, f.ID)
		}
		return out
	}
	if got := ids(s.Drift); !slices.Equal(got, []string{"F-1", "F-6"}) {
		t.Errorf("Drift = %v", got)
	}
	if got := ids(s.NewRule); !slices.Equal(got, []string{"F-2", "F-4", "F-7"}) {
		t.Errorf("NewRule = %v", got)
	}
	if got := ids(s.Preview); !slices.Equal(got, []string{"F-3", "F-5"}) {
		t.Errorf("Preview = %v (a preview rule new to the baseline is preview, not new)", got)
	}
	if !slices.Equal(s.NewRuleIDs, []string{"empty-branch", "unused-variable"}) {
		t.Errorf("NewRuleIDs = %v", s.NewRuleIDs)
	}

	d := ComputeDrift("f", &models.AnalysisReport{Findings: fs}, []string{})
	s.Annotate(d)
	if d.NewFromNewRules != 3 || d.Preview != 2 || !slices.Equal(d.NewRuleIDs, s.NewRuleIDs) || len(d.New) != len(fs) {
		t.Errorf("Annotate: %+v", d)
	}
	b, _ := json.Marshal(ComputeDrift("f", &models.AnalysisReport{}, nil))
	for _, k := range []string{"newRuleIds", "newFromNewRules", "preview"} {
		if strings.Contains(string(b), k) {
			t.Errorf("an unannotated drift must keep its wire shape, found %s in %s", k, b)
		}
	}

	if s := SplitDrift(nil, age); s.Drift != nil || s.NewRule != nil || s.Preview != nil || s.NewRuleIDs != nil {
		t.Errorf("nil input: %+v", s)
	}
}

// TestHealthOf_EqualsHealthScore: over the findings-golden corpus, HealthOf of a
// report's own findings is its stored health score, so a like-for-like subset
// compares with stored snapshots.
func TestHealthOf_EqualsHealthScore(t *testing.T) {
	if testing.Short() {
		t.Skip("corpus run")
	}
	for _, in := range findingsGoldenInputs(t) {
		doc := in.build(t)
		for _, settings := range []*models.AppSettings{nil, models.DefaultSettings()} {
			r := RunAnalysis(doc, AllRules(), settings, nil)
			if got := HealthOf(r.Findings, r.Metrics); got != r.Metrics.HealthScore {
				t.Errorf("%s: HealthOf %d, HealthScore %d", in.name, got, r.Metrics.HealthScore)
			}
		}
	}
}

func TestHealthOf_ComplexityTermsAndPreview(t *testing.T) {
	withPreviewRule(t, "wait-zero")
	fs := []models.Finding{
		{RuleID: "dead-code", Severity: models.SeverityError},
		{RuleID: "dead-code", Severity: models.SeverityWarning},
		{RuleID: "dead-code", Severity: models.SeverityInfo},
		{RuleID: "dead-code", Severity: "critical"},
		{RuleID: "wait-zero", Severity: models.SeverityError},
		{RuleID: "dead-code", Severity: models.SeverityError, Stage: "preview"},
	}
	// The unknown "critical" costs an error's 12.
	if got := HealthOf(fs, nil); got != 100-12-4-1-12 {
		t.Errorf("no metrics: %d", got)
	}
	m := &models.FlowMetrics{MaxCyclomatic: 22, MaxCognitive: 32, CircularDependencies: []string{"sf"}}
	if got := HealthOf(fs, m); got != 100-12-4-1-12-4-3-10 {
		t.Errorf("with complexity terms: %d", got)
	}
	if got := HealthOf(append(fs, make([]models.Finding, 10)...), &models.FlowMetrics{MaxCyclomatic: 200}); got != 0 {
		t.Errorf("clamped: %d", got)
	}
}

// TestHealthScore_ExcludesPreview: a rule flipped to preview leaves the health
// score where it would be without its findings, which stay in the list and in
// Stats.
func TestHealthScore_ExcludesPreview(t *testing.T) {
	doc := parseGoldenText(t, goldenSuppressionsFlow, "Main.txt")
	before := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	const id = "unused-variable"
	var without []models.Finding
	n := 0
	for _, f := range before.Findings {
		if f.RuleID == id {
			n++
			continue
		}
		without = append(without, f)
	}
	if n == 0 {
		t.Fatalf("fixture rot: no %s finding", id)
	}

	restore := SetRuleMetaForTesting(id, previewMetaOf(t, id))
	after := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	restore()

	if want := HealthOf(without, before.Metrics); after.Metrics.HealthScore != want || want <= before.Metrics.HealthScore {
		t.Errorf("health with %s in preview: %d, want %d (stable: %d)", id, after.Metrics.HealthScore, want, before.Metrics.HealthScore)
	}
	if after.Stats != before.Stats || len(after.Findings) != len(before.Findings) {
		t.Errorf("preview findings stay in the list and in Stats: %+v vs %+v", after.Stats, before.Stats)
	}
	stamped := 0
	for _, f := range after.Findings {
		if f.Stage == "preview" {
			stamped++
		}
	}
	if stamped != n {
		t.Errorf("%d findings stamped preview, want %d", stamped, n)
	}
}
