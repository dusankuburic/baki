package analyzer

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"pad-core/models"
)

// buildCircularProbeFlow is the call graph A→{B,C,E}, B→C, C→A plus a
// self-calling D. A depth-first search that keeps only back-edge paths reports
// B only when it happens to visit B before C, so B's finding used to flap.
func buildCircularProbeFlow() *models.FlowDocument {
	calls := map[string][]string{"A": {"B", "C", "E"}, "B": {"C"}, "C": {"A"}, "D": {"D"}, "E": nil}
	var subflows []models.Subflow
	line := 0
	for _, name := range []string{"A", "B", "C", "D", "E"} {
		sfID := "sf" + name
		var blocks []models.Block
		line++
		b := makeBlock("b"+name+"0", "Click "+name, models.BlockTypeAction, "WebAutomation.Click.Click", 0)
		b.SubflowID, b.LineNumber = sfID, line
		blocks = append(blocks, *b)
		for _, target := range calls[name] {
			line++
			c := makeBlock("b"+name+"call"+target, "Call "+target, models.BlockTypeAction, "CALL", 0)
			c.SubflowID, c.LineNumber = sfID, line
			blocks = append(blocks, *c)
		}
		subflows = append(subflows, models.Subflow{ID: sfID, Name: name, Blocks: blocks})
	}
	return goldenIndexed(&models.FlowDocument{ID: "probe-circular", Name: "probe", Subflows: subflows})
}

// buildMultiPropertyProbeFlow gives each property-scanning rule a block on
// which several properties qualify, so the reported property (and with it the
// fingerprint, the description and the order of a block's findings) used to
// follow map iteration order.
func buildMultiPropertyProbeFlow() *models.FlowDocument {
	specs := []struct {
		rawType string
		props   map[string]string
	}{
		{"WebAutomation.GoToWebPage", map[string]string{
			"Url": "https://api.example.com/v1", "Endpoint": "https://api.example.com/v1",
			"ApiAddress": "https://other.example.com/x", "Address": "http://insecure.example.com",
			"Uri": "http://plain.example.com", "Host": "10.0.0.5", "ServerAddress": "10.0.0.5",
		}},
		{"File.Copy", map[string]string{
			"Files": `C:\data\in.txt`, "Destination": `C:\data\in.txt`, "SourceFolder": `C:\data\in.txt`,
		}},
		{"File.Move", map[string]string{"Files": "%Source%", "Destination": "%Target%", "Folder": "%Dir%"}},
		{"Database.ExecuteSqlStatement", map[string]string{
			"Sql": "SELECT * FROM t WHERE a = %A%", "Statement": "SELECT %B%", "Query": "DELETE FROM t WHERE c = %C%",
		}},
		{"UIAutomation.Click", map[string]string{"X": "10", "Y": "20", "Left": "5"}},
		{"System.RunApplication", map[string]string{"ApplicationPath": "cmd.exe", "CommandLineArguments": "/c %Arg%", "WorkingFolder": "%Dir%"}},
		{"Email.SendEmail", map[string]string{
			"Password": "hunter22", "SmtpPassword": "s3cretpass", "Body": `token: "abcdefghijklmnopqrstu"`,
		}},
	}
	var blocks []models.Block
	for i, s := range specs {
		b := makeBlock(fmt.Sprintf("p%d", i), s.rawType, models.BlockTypeAction, s.rawType, 0)
		b.SubflowID, b.LineNumber, b.Properties = "sf1", i+1, s.props
		blocks = append(blocks, *b)
	}
	return goldenIndexed(&models.FlowDocument{ID: "probe-props", Name: "probe", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}}})
}

// goldenRenderAnalysis is everything a run produces, position-based (see the
// findings golden), for run-to-run comparison.
func goldenRenderAnalysis(t *testing.T, doc *models.FlowDocument) string {
	l := newGoldenLocator(doc)
	settings := models.DefaultSettings()
	r := RunAnalysis(doc, AllRules(), settings, nil)
	var sb strings.Builder
	sb.WriteString(goldenJSON(t, r.Stats) + "\n")
	sb.WriteString(goldenMetrics(t, l, r.Metrics))
	for _, f := range r.Findings {
		sb.WriteString(goldenFindingHead(l, f) + "\n" + goldenFindingBody(t, l, f))
	}
	for _, e := range SuppressionInventory(doc, AllRules(), settings) {
		sb.WriteString(l.ids.Replace(goldenJSON(t, e)) + "\n")
	}
	return sb.String()
}

func requireDeterministic(t *testing.T, name string, runs int, build func() *models.FlowDocument) {
	t.Helper()
	first := goldenRenderAnalysis(t, build())
	for i := 1; i < runs; i++ {
		if got := goldenRenderAnalysis(t, build()); got != first {
			t.Fatalf("%s: run %d differs from run 1:\n%s", name, i+1, firstGoldenDiff(first, got))
		}
	}
}

// TestAnalysisDeterministic_Probes runs each probe many times: map order is
// randomized per range loop, so a map-order dependence shows up within a few
// dozen runs (B was missed in 260 of 300 runs before the SCC fix).
func TestAnalysisDeterministic_Probes(t *testing.T) {
	requireDeterministic(t, "circular", 300, buildCircularProbeFlow)
	requireDeterministic(t, "multi-property", 300, buildMultiPropertyProbeFlow)
}

func TestAnalysisDeterministic_Corpus(t *testing.T) {
	if testing.Short() {
		t.Skip("full corpus; skipped in -short")
	}
	for _, in := range findingsGoldenInputs(t) {
		requireDeterministic(t, in.name, 8, func() *models.FlowDocument { return in.build(t) })
	}
}

// TestCircularDeps_EverySubflowOnACycle: every member of a strongly connected
// component and a self-caller is reported once, in document order; the
// acyclic callee is not.
func TestCircularDeps_EverySubflowOnACycle(t *testing.T) {
	doc := buildCircularProbeFlow()
	r := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	l := newGoldenLocator(doc)
	var circular, flagged []string
	for _, id := range r.Metrics.CircularDependencies {
		circular = append(circular, l.subflowName(id))
	}
	for _, f := range r.Findings {
		if f.RuleID == "circular-subflow-dependency" {
			flagged = append(flagged, l.subflowName(f.SubflowID))
		}
	}
	want := []string{"A", "B", "C", "D"}
	if !slices.Equal(circular, want) || !slices.Equal(flagged, want) {
		t.Fatalf("circular metrics %v, findings %v; want both %v", circular, flagged, want)
	}
}

// TestPropertyRules_SortedKeyWins pins which property each property-scanning
// rule reports when several qualify: the first in sorted key order, and for
// value-deduplicating rules the first key holding each value.
func TestPropertyRules_SortedKeyWins(t *testing.T) {
	r := RunAnalysis(buildMultiPropertyProbeFlow(), AllRules(), models.DefaultSettings(), nil)
	got := map[string][]string{}
	for _, f := range r.Findings {
		if p, ok := f.Metadata["property"].(string); ok {
			got[f.RuleID] = append(got[f.RuleID], p)
		}
	}
	want := map[string][]string{
		"hardcoded-url":            {"Address", "ApiAddress", "Endpoint"},
		"hardcoded-filepath":       {"Destination"},
		"hardcoded-ip":             {"Host"},
		"insecure-http-url":        {"Address"},
		"path-traversal-risk":      {"Destination"},
		"sql-injection-risk":       {"Query"},
		"hardcoded-ui-coordinates": {"Left"},
	}
	for rule, props := range want {
		if !slices.Equal(got[rule], props) {
			t.Errorf("%s reported properties %v, want %v", rule, got[rule], props)
		}
	}
	cred := got["hardcoded-credential"]
	if len(cred) < 2 || !slices.IsSorted(cred) {
		t.Errorf("hardcoded-credential properties %v: want several, in sorted key order", cred)
	}
}

// TestSanitizeCommandVarsPatch_SortedKeyOrder: the sanitize-command-vars ops
// follow sorted property-key order, so the previewed patch is stable and
// ApplyPatch gives one result even when refs overlap (%X%A%Y% contains %A%).
func TestSanitizeCommandVarsPatch_SortedKeyOrder(t *testing.T) {
	b := &models.Block{LineNumber: 1, Properties: map[string]string{
		"WorkingFolder":        "%Dir%",
		"CommandLineArguments": "/c %Arg% %X%A%Y%",
		"ApplicationPath":      "%App% %A%",
	}}
	src := `System.RunApplication.RunApplication ApplicationPath: $'''%App% %A%''' CommandLineArguments: $'''/c %Arg% %X%A%Y%''' WorkingFolder: $'''%Dir%'''`
	want := []string{"%App%", "%A%", "%Arg%", "%X%", "%Y%", "%Dir%"}
	var firstText string
	for i := 0; i < 200; i++ {
		p, err := PatchForFix(b, "sanitize-command-vars", "command-injection-risk", "", "")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, op := range p.Ops {
			got = append(got, op.Old)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("run %d: ops %v, want %v", i+1, got, want)
		}
		text := ApplyPatch(src, p)
		if i == 0 {
			firstText = text
		} else if text != firstText {
			t.Fatalf("run %d: applied text %q differs from run 1 %q", i+1, text, firstText)
		}
	}
}
