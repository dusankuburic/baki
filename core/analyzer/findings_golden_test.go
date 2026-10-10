package analyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

var updateFindings = flag.Bool("update-findings", false, "rewrite core/analyzer/testdata/findings/*.golden from the current analyzer output")

// The findings golden pins the whole analysis output (finding order, IDs,
// fingerprints, text, metadata, stats, rule profiles without timings, metrics,
// the suppression inventory and progress) for every corpus input under six
// settings variants. Refresh with -update-findings only for an intended change
// and explain every changed line.
const (
	findingsGoldenDir = "testdata/findings"
	goldenSamplesDir  = "../parser/testdata/samples"
	// Above this many findings a variant prints per-rule counts and digests
	// instead of one entry per finding, to keep the golden files reviewable.
	findingsGoldenDetailCap = 300
	// Above this many subflows the per-subflow metrics are digested.
	findingsGoldenSubflowCap = 60
)

type findingsGoldenInput struct {
	name  string
	build func(t *testing.T) *models.FlowDocument
}

type findingsGoldenVariant struct {
	name     string
	settings func() *models.AppSettings
	rules    func() []Rule
}

// goldenSuppressionsFlow exercises inline pad-ignore directives and the
// suppression inventory, which no corpus sample does: a live directive, a
// stale one, an "all rules" one, the empty-branch directive (the finding
// sits on the ELSE while the rule is dispatched on the IF; the inventory
// reported it stale before it shared the engine's dispatcher) and a
// subflow-header directive.
const goldenSuppressionsFlow = `#Region "Main"
    # pad-ignore[unhandled-error]
    File.ReadTextFromFile.ReadText File: $'''C:\data\in.txt''' Encoding: File.TextFileEncoding.UTF8 Content=> FileContents
    # pad-ignore[missing-delay]
    Display.ShowMessageBox Message: $'''Read %FileContents%'''
    # pad-ignore
    WebAutomation.NavigateTo Url: $'''http://example.com/api''' BrowserInstance: %Browser%
    IF %FileContents% = $'''x''' THEN
        SET Seen TO True
        # pad-ignore[empty-branch]
    ELSE
    END
    CALL Helper
#EndRegion
#Region "Helper"
    # pad-ignore[subflow-no-error-handler]
    File.WriteText File: $'''C:\data\out.txt''' TextToWrite: %FileContents% AppendNewLine: True IfFileExists: File.IfFileExists.Overwrite
    File.Delete Files: $'''C:\data\tmp.txt'''
    File.Delete Files: $'''C:\data\a.txt'''
    Display.ShowMessageBox Message: $'''done'''
#EndRegion
`

func parseGoldenText(t *testing.T, src, name string) *models.FlowDocument {
	t.Helper()
	doc, err := parser.ParseText(src, name, int64(len(src)))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return doc
}

// goldenIndexed returns a synthetic builder's document with the lookup maps the
// parser always fills, so fingerprints are content-derived as in production.
func goldenIndexed(doc *models.FlowDocument) *models.FlowDocument {
	doc.RebuildIndexes()
	return doc
}

func findingsGoldenInputs(t *testing.T) []findingsGoldenInput {
	t.Helper()
	var files []string
	dirs := map[string]bool{}
	err := filepath.Walk(goldenSamplesDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		lower := strings.ToLower(p)
		if strings.HasSuffix(lower, ".txt") || strings.HasSuffix(lower, ".pad") {
			files = append(files, p)
			if dir := filepath.Dir(p); filepath.Clean(dir) != filepath.Clean(goldenSamplesDir) && strings.HasSuffix(lower, ".txt") {
				dirs[dir] = true
			}
		}
		return nil
	})
	if err != nil || len(files) < 10 {
		t.Fatalf("samples walk: %v (%d files)", err, len(files))
	}
	sort.Strings(files)

	slug := func(rel string) string {
		rel = filepath.ToSlash(rel)
		rel = strings.TrimSuffix(strings.TrimSuffix(rel, ".txt"), ".pad")
		return strings.NewReplacer("/", "_", " ", "_").Replace(rel)
	}

	var inputs []findingsGoldenInput
	for _, p := range files {
		rel, _ := filepath.Rel(goldenSamplesDir, p)
		inputs = append(inputs, findingsGoldenInput{name: "sample_" + slug(rel), build: func(t *testing.T) *models.FlowDocument {
			data, err := os.ReadFile(p) // #nosec G304 -- test corpus
			if err != nil {
				t.Fatal(err)
			}
			return parseGoldenText(t, string(data), filepath.Base(p))
		}})
	}
	var dirList []string
	for d := range dirs {
		dirList = append(dirList, d)
	}
	sort.Strings(dirList)
	for _, d := range dirList {
		rel, _ := filepath.Rel(goldenSamplesDir, d)
		inputs = append(inputs, findingsGoldenInput{name: "folder_" + slug(rel), build: func(t *testing.T) *models.FlowDocument {
			doc, err := parser.ParseFolder(d)
			if err != nil {
				t.Fatalf("ParseFolder %s: %v", d, err)
			}
			return doc
		}})
	}

	inputs = append(inputs,
		findingsGoldenInput{name: "mixed_flow", build: func(t *testing.T) *models.FlowDocument {
			data, err := os.ReadFile("testdata/mixed_flow.txt")
			if err != nil {
				t.Fatal(err)
			}
			return parseGoldenText(t, string(data), "mixed_flow.txt")
		}},
		findingsGoldenInput{name: "inline_suppressions", build: func(t *testing.T) *models.FlowDocument {
			return parseGoldenText(t, goldenSuppressionsFlow, "Main.txt")
		}},
		findingsGoldenInput{name: "synthetic_large_2000", build: func(*testing.T) *models.FlowDocument {
			return goldenIndexed(buildLargeFlow(2000))
		}},
		findingsGoldenInput{name: "synthetic_many_subflows_200", build: func(*testing.T) *models.FlowDocument {
			return goldenIndexed(buildManySubflowsFlow(200))
		}},
		findingsGoldenInput{name: "synthetic_label_heavy", build: func(*testing.T) *models.FlowDocument {
			return goldenIndexed(buildLabelHeavyFlow(130, 4))
		}},
		findingsGoldenInput{name: "synthetic_nested_loop", build: func(*testing.T) *models.FlowDocument {
			return goldenIndexed(buildNestedLoopFlow(5, 2))
		}},
		findingsGoldenInput{name: "synthetic_goto", build: func(*testing.T) *models.FlowDocument {
			return goldenIndexed(buildGotoFlow(12))
		}},
		findingsGoldenInput{name: "synthetic_resource_open", build: func(*testing.T) *models.FlowDocument {
			return goldenIndexed(buildResourceOpenFlow(12))
		}},
		findingsGoldenInput{name: "probe_circular", build: func(*testing.T) *models.FlowDocument {
			return buildCircularProbeFlow()
		}},
		findingsGoldenInput{name: "probe_multi_property", build: func(*testing.T) *models.FlowDocument {
			return buildMultiPropertyProbeFlow()
		}},
	)
	return inputs
}

func findingsGoldenVariants() []findingsGoldenVariant {
	withRules := func(edit func(rules map[string]models.RuleConfig)) func() *models.AppSettings {
		return func() *models.AppSettings {
			s := models.DefaultSettings()
			edit(s.Analysis.Rules)
			return s
		}
	}
	return []findingsGoldenVariant{
		{name: "default", settings: models.DefaultSettings, rules: AllRules},
		{name: "nil-settings", settings: func() *models.AppSettings { return nil }, rules: AllRules},
		{name: "thresholds", rules: AllRules, settings: withRules(func(r map[string]models.RuleConfig) {
			set := func(id, key string, v any) {
				rc := r[id]
				rc.Options = map[string]interface{}{key: v}
				r[id] = rc
			}
			set("deep-nesting", "maxDepth", 2)
			set("duplicate-action", "minRepeats", 2)
			set("wide-loop", "maxBlocks", 3)
			set("large-subflow", "maxBlocks", float64(5)) // JSON-decoded settings carry float64
		})},
		{name: "disabled", rules: AllRules, settings: withRules(func(r map[string]models.RuleConfig) {
			for _, id := range []string{"unhandled-error", "dead-data", "hardcoded-credential", "parse-error",
				"circular-subflow-dependency", "missing-delay", "subflow-no-error-handler", "uninitialized-variable"} {
				rc := r[id]
				rc.Enabled = false
				r[id] = rc
			}
		})},
		{name: "severity", rules: AllRules, settings: withRules(func(r map[string]models.RuleConfig) {
			for id, sev := range map[string]string{
				"unhandled-error": "error", "missing-delay": "warning", "uninitialized-variable": "info",
				"subflow-no-error-handler": "error", "hardcoded-url": "warning", "todo-in-comment": "error",
				"dead-code": "warning", "parse-error": "warning",
			} {
				rc := r[id]
				rc.Severity = sev
				r[id] = rc
			}
		})},
		{name: "autofixable", settings: models.DefaultSettings, rules: func() []Rule { return AutoFixableRules(AllRules()) }},
	}
}

// goldenLocator names blocks and subflows by position, since the parser mints
// fresh UUIDs on every parse.
type goldenLocator struct {
	block   map[string]string
	subflow map[string]string
	ids     *strings.Replacer
}

func newGoldenLocator(doc *models.FlowDocument) *goldenLocator {
	l := &goldenLocator{block: map[string]string{}, subflow: map[string]string{}}
	names := map[string]int{}
	for i := range doc.Subflows {
		names[doc.Subflows[i].Name]++
	}
	var pairs []string
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		label := sf.Name
		if names[sf.Name] > 1 || sf.Name == "" {
			label = fmt.Sprintf("%s[%d]", sf.Name, i)
		}
		l.subflow[sf.ID] = label
		pairs = append(pairs, sf.ID, "{sf:"+label+"}")
		n := 0
		var walk func(bs []models.Block)
		walk = func(bs []models.Block) {
			for j := range bs {
				p := fmt.Sprintf("%s#%d", label, n)
				n++
				l.block[bs[j].ID] = p
				pairs = append(pairs, bs[j].ID, "{block:"+p+"}")
				walk(bs[j].Children)
			}
		}
		walk(sf.Blocks)
	}
	l.ids = strings.NewReplacer(pairs...)
	return l
}

func (l *goldenLocator) blockPath(id string) string {
	if id == "" {
		return "-"
	}
	if p, ok := l.block[id]; ok {
		return p
	}
	return "?" + id
}

func (l *goldenLocator) subflowName(id string) string {
	if id == "" {
		return "-"
	}
	if n, ok := l.subflow[id]; ok {
		return n
	}
	return "?" + id
}

func goldenOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func goldenJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// goldenJSONWithout marshals v with the given top-level keys dropped (keys
// come out sorted).
func goldenJSONWithout(t *testing.T, v any, drop ...string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(goldenJSON(t, v)), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range drop {
		delete(m, k)
	}
	return goldenJSON(t, m)
}

func goldenDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// goldenFindingHead is the one-line identity of a finding; goldenFindingBody is the text
// the rule wrote. Together they are everything a finding carries except
// Suggestion and AutoFixHint (rule constants).
func goldenFindingHead(l *goldenLocator, f models.Finding) string {
	head := fmt.Sprintf("%s %s/%s %s [%s] fix=%s @%s sf=%s fp=%s", f.ID, f.Severity, goldenOrDash(string(f.Confidence)),
		f.RuleID, goldenOrDash(f.Category), goldenOrDash(f.AutoFix), l.blockPath(f.BlockID), l.subflowName(f.SubflowID), goldenOrDash(f.Fingerprint))
	if f.Stage != "" {
		head += " stage=" + f.Stage
	}
	return head
}

func goldenFindingBody(t *testing.T, l *goldenLocator, f models.Finding) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "    title: %s\n", l.ids.Replace(f.Title))
	fmt.Fprintf(&sb, "    desc: %s\n", l.ids.Replace(strings.ReplaceAll(f.Description, "\n", `\n`)))
	if len(f.Metadata) > 0 {
		fmt.Fprintf(&sb, "    meta: %s\n", l.ids.Replace(goldenJSON(t, f.Metadata)))
	}
	return sb.String()
}

// goldenFindingContent is a finding minus ID and severity, the two fields a
// settings variant changes without changing what the rule found.
func goldenFindingContent(t *testing.T, l *goldenLocator, f models.Finding) string {
	f.ID, f.Severity = "", ""
	return goldenFindingHead(l, f) + "\n" + goldenFindingBody(t, l, f)
}

type goldenRun struct {
	report    *models.AnalysisReport
	inventory []SuppressionEntry
	events    int
	lastCur   int
	lastTotal int
	lastName  string
	sharded   bool
}

func runGoldenVariant(doc *models.FlowDocument, v findingsGoldenVariant) goldenRun {
	var run goldenRun
	settings, rules := v.settings(), v.rules()
	// The engine shards flows of 256+ non-END blocks across workers; progress
	// events then depend on scheduling, so only their bounds are checked.
	n := 0
	walkBlocks(doc, func(b *models.Block) {
		if b.Type != models.BlockTypeEnd {
			n++
		}
	})
	run.sharded = n >= 256
	run.report = RunAnalysis(doc, rules, settings, func(cur, total int, name string) {
		run.events++
		run.lastCur, run.lastTotal, run.lastName = cur, total, name
	})
	run.inventory = SuppressionInventory(doc, rules, settings)
	return run
}

// goldenMetrics renders the metrics with subflow IDs replaced by names; the
// per-subflow lines are digested for very wide flows.
func goldenMetrics(t *testing.T, l *goldenLocator, m *models.FlowMetrics) string {
	if m == nil {
		return "metrics: nil\n"
	}
	var sb strings.Builder
	circ := make([]string, 0, len(m.CircularDependencies))
	for _, id := range m.CircularDependencies {
		circ = append(circ, l.subflowName(id))
	}
	fmt.Fprintf(&sb, "metrics: %s circular=%s\n", goldenJSONWithout(t, m, "subflows", "circularDependencies"), goldenJSON(t, circ))
	var lines []string
	for _, sm := range m.Subflows {
		lines = append(lines, fmt.Sprintf("  subflow %s: %s\n", l.subflowName(sm.SubflowID),
			goldenJSONWithout(t, sm, "subflowId", "subflowName")))
	}
	if len(lines) > findingsGoldenSubflowCap {
		fmt.Fprintf(&sb, "  subflows: %d digest=%s\n", len(lines), goldenDigest(strings.Join(lines, "")))
	} else {
		sb.WriteString(strings.Join(lines, ""))
	}
	return sb.String()
}

func goldenProfiles(r *models.AnalysisReport) string {
	var order, findings []string
	blocks := map[int]bool{}
	for _, p := range r.RuleProfiles {
		order = append(order, p.RuleID+"/"+p.RuleName)
		blocks[p.BlocksChecked] = true
		if p.FindingCount > 0 {
			findings = append(findings, fmt.Sprintf("%s=%d", p.RuleID, p.FindingCount))
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "profiles: rules=%d order=%s", len(r.RuleProfiles), goldenDigest(strings.Join(order, "\n")))
	if len(blocks) == 1 {
		for b := range blocks {
			fmt.Fprintf(&sb, " blocksChecked=%d", b)
		}
	} else {
		sb.WriteString(" blocksChecked:")
		for _, p := range r.RuleProfiles {
			fmt.Fprintf(&sb, " %s=%d", p.RuleID, p.BlocksChecked)
		}
	}
	sb.WriteString("\n")
	if len(findings) > 0 {
		fmt.Fprintf(&sb, "  raw findings: %s\n", strings.Join(findings, " "))
	}
	return sb.String()
}

// goldenBase is the default variant, rendered position-based so later
// variants (which re-parse the input) can be compared with it.
type goldenBase struct {
	health   int
	metrics  string
	content  map[string]bool
	order    []string
	severity []models.Severity
}

func renderGoldenVariant(t *testing.T, l *goldenLocator, v findingsGoldenVariant, run goldenRun, base *goldenBase) string {
	r := run.report
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n=== %s\n", v.name)
	fmt.Fprintf(&sb, "stats: %s\n", goldenJSON(t, r.Stats))
	if run.sharded {
		if run.events < 1 || run.events > 101 {
			t.Errorf("%s: %d progress events, want 1..101", v.name, run.events)
		}
		fmt.Fprintf(&sb, "progress: events=sharded final=%d/%d last=%q\n", run.lastCur, run.lastTotal, run.lastName)
	} else {
		fmt.Fprintf(&sb, "progress: events=%d final=%d/%d last=%q\n", run.events, run.lastCur, run.lastTotal, run.lastName)
	}
	metrics := goldenMetrics(t, l, r.Metrics)
	if base != nil && r.Metrics != nil {
		m := *r.Metrics
		m.HealthScore = base.health
		if goldenMetrics(t, l, &m) == base.metrics {
			metrics = fmt.Sprintf("metrics: as default, healthScore=%d\n", r.Metrics.HealthScore)
		}
	}
	sb.WriteString(metrics)
	sb.WriteString(goldenProfiles(r))

	if len(run.inventory) == 0 {
		sb.WriteString("inventory: none\n")
	} else {
		fmt.Fprintf(&sb, "inventory: %d\n", len(run.inventory))
		for _, e := range run.inventory {
			fmt.Fprintf(&sb, "  %s\n", l.ids.Replace(goldenJSON(t, e)))
		}
	}

	fmt.Fprintf(&sb, "findings: %d\n", len(r.Findings))
	if len(r.Findings) > findingsGoldenDetailCap {
		var full, ordered, byRule []string
		counts := map[string]int{}
		for _, f := range r.Findings {
			full = append(full, goldenFindingHead(l, f)+"\n"+goldenFindingBody(t, l, f))
			ordered = append(ordered, f.Fingerprint)
			if counts[f.RuleID] == 0 {
				byRule = append(byRule, f.RuleID)
			}
			counts[f.RuleID]++
		}
		for i, id := range byRule {
			byRule[i] = fmt.Sprintf("%s=%d", id, counts[id])
		}
		sorted := append([]string(nil), ordered...)
		sort.Strings(sorted)
		fmt.Fprintf(&sb, "  by rule: %s\n", strings.Join(byRule, " "))
		fmt.Fprintf(&sb, "  digest: full=%s ordered-fingerprints=%s fingerprint-set=%s\n",
			goldenDigest(strings.Join(full, "")), goldenDigest(strings.Join(ordered, "\n")), goldenDigest(strings.Join(sorted, "\n")))
		return sb.String()
	}
	if base != nil && len(r.Findings) == len(base.order) {
		same := true
		var sevs []string
		for i, f := range r.Findings {
			if goldenFindingContent(t, l, f) != base.order[i] {
				same = false
				break
			}
			if f.Severity != base.severity[i] {
				sevs = append(sevs, fmt.Sprintf("%s %s->%s", f.ID, base.severity[i], f.Severity))
			}
		}
		if same {
			if len(sevs) == 0 {
				sevs = []string{"none"}
			}
			fmt.Fprintf(&sb, "  as default; severity changes: %s\n", strings.Join(sevs, ", "))
			return sb.String()
		}
	}
	for _, f := range r.Findings {
		sb.WriteString(goldenFindingHead(l, f) + "\n")
		if base == nil || !base.content[goldenFindingContent(t, l, f)] {
			sb.WriteString(goldenFindingBody(t, l, f))
		}
	}
	return sb.String()
}

func renderFindingsGolden(t *testing.T, in findingsGoldenInput) string {
	var sb strings.Builder
	sb.WriteString("# Analysis golden (core/analyzer/findings_golden_test.go). Blocks are <subflow>#<pre-order index>.\n")
	sb.WriteString("# Later variants print \"as default\" plus severity changes when only severities differ; otherwise they\n")
	sb.WriteString("# show title/desc/meta only for findings whose content differs from every default finding.\n")
	var base *goldenBase
	for i, v := range findingsGoldenVariants() {
		doc := in.build(t)
		if i == 0 {
			blocks := 0
			walkBlocks(doc, func(*models.Block) { blocks++ })
			fmt.Fprintf(&sb, "input: %s\ndocument: subflows=%d blocks=%d parseErrors=%d folder=%t\n",
				in.name, len(doc.Subflows), blocks, len(doc.ParseErrors), doc.IsFolder)
		}
		l := newGoldenLocator(doc)
		run := runGoldenVariant(doc, v)
		sb.WriteString(renderGoldenVariant(t, l, v, run, base))
		if i == 0 {
			base = &goldenBase{content: map[string]bool{}, metrics: goldenMetrics(t, l, run.report.Metrics)}
			if run.report.Metrics != nil {
				base.health = run.report.Metrics.HealthScore
			}
			for _, f := range run.report.Findings {
				c := goldenFindingContent(t, l, f)
				base.content[c] = true
				base.order = append(base.order, c)
				base.severity = append(base.severity, f.Severity)
			}
		}
	}
	return sb.String()
}

func TestFindingsGolden(t *testing.T) {
	if testing.Short() {
		t.Skip("full-corpus golden; skipped in -short")
	}
	inputs := findingsGoldenInputs(t)
	want := map[string]bool{}
	for _, in := range inputs {
		want[in.name+".golden"] = true
		t.Run(in.name, func(t *testing.T) {
			got := renderFindingsGolden(t, in)
			path := filepath.Join(findingsGoldenDir, in.name+".golden")
			if *updateFindings {
				if err := os.MkdirAll(findingsGoldenDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			wantBytes, err := os.ReadFile(path) // #nosec G304 -- test golden
			if err != nil {
				t.Fatalf("%v (run with -update-findings to create it)", err)
			}
			if d := firstGoldenDiff(string(wantBytes), got); d != "" {
				t.Errorf("analysis output differs from %s (refresh with -update-findings only for an intended change):\n%s", path, d)
			}
		})
	}
	entries, err := os.ReadDir(findingsGoldenDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".golden") && !want[e.Name()] {
			if *updateFindings {
				_ = os.Remove(filepath.Join(findingsGoldenDir, e.Name()))
				continue
			}
			t.Errorf("stale golden %s has no input (run with -update-findings to remove it)", e.Name())
		}
	}
}

// firstGoldenDiff reports the first differing line with a little context, or
// "" when equal. CRs are ignored so a CRLF checkout still compares.
func firstGoldenDiff(want, got string) string {
	want = strings.ReplaceAll(want, "\r\n", "\n")
	if want == got {
		return ""
	}
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			lo := i - 3
			if lo < 0 {
				lo = 0
			}
			var sb strings.Builder
			for j := lo; j < i && j < len(g); j++ {
				fmt.Fprintf(&sb, "   %d  %s\n", j+1, g[j])
			}
			fmt.Fprintf(&sb, "-  %d  %s\n+  %d  %s\n", i+1, wl, i+1, gl)
			fmt.Fprintf(&sb, "(%d lines wanted, %d got)", len(w), len(g))
			return sb.String()
		}
	}
	return "(differs only in length)"
}
