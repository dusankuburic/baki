package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// libParse parses text as the flow file at path (a path-backed doc, so its
// StableID survives re-parsing).
func libParse(t testing.TB, path, text string) *models.FlowDocument {
	t.Helper()
	doc, err := parser.ParseText(text, filepath.Base(path), int64(len(text)))
	if err != nil {
		t.Fatal(err)
	}
	doc.FilePath = path
	return doc
}

// dupTestFlow is a flow with a subflow named shared whose body is the same in
// every flow, and a subflow named own whose body depends on its name.
func dupTestFlow(shared, own string) string {
	return `#Region "` + shared + `"
    COMMENT  Log in with retries
    Display.ShowMessageBox Message: $'''Logging in''' Icon: Display.Icon.None
    SET Attempts TO 0
    LOOP WHILE %Attempts% < 3
        Variables.IncreaseVariable Value: 1 Name: Attempts
        WebAutomation.Click.Click BrowserInstance: %Browser% Control: appmask['Login']
    END
#EndRegion
#Region "` + own + `"
    Display.ShowMessageBox Message: $'''Own ` + own + `''' Icon: Display.Icon.None
    SET X TO 1
    SET Y TO 2
#EndRegion
`
}

var dupLibRule = &DuplicateSubflowAcrossFlowsRule{}

// dupAnalyze runs AnalyzeLibrary (default: the duplicate rule alone) and fails
// when a rule was skipped: a recovered panic would look like "no findings".
func dupAnalyze(t *testing.T, docs []*models.FlowDocument, rules ...LibraryRule) *models.LibraryReport {
	t.Helper()
	if len(rules) == 0 {
		rules = []LibraryRule{dupLibRule}
	}
	rep := AnalyzeLibrary(context.Background(), docs, rules, nil)
	if rep.RulesSkipped != 0 {
		t.Fatalf("%d library rules skipped", rep.RulesSkipped)
	}
	return rep
}

func dupLibBlock(id, rawType, name string, props map[string]string, children ...models.Block) models.Block {
	bt := models.BlockTypeAction
	switch rawType {
	case "COMMENT":
		bt = models.BlockTypeComment
	case "END":
		bt = models.BlockTypeEnd
	case "LOOP":
		bt = models.BlockTypeLoop
	}
	return models.Block{ID: id, Name: name, Type: bt, RawType: rawType, Properties: props, Children: children}
}

func dupLibHash(t *testing.T, blocks ...models.Block) (string, int) {
	t.Helper()
	return subflowBodyHash(&models.Subflow{ID: "sf", Name: "S", Blocks: blocks})
}

func TestSubflowBodyHash_IgnoresWhatDoesNotRun(t *testing.T) {
	base, n := dupLibHash(t,
		dupLibBlock("1", "Display.ShowMessageBox", "Show", map[string]string{"Message": "hi", "Icon": "None"}),
		dupLibBlock("2", "LOOP", "Loop", map[string]string{"Count": "3"},
			dupLibBlock("3", "WebAutomation.Click.Click", "Click", nil),
			dupLibBlock("4", "END", "End", nil)),
	)
	if n != 3 {
		t.Fatalf("counted %d blocks, want 3 (END left out)", n)
	}
	other := []models.Block{
		dupLibBlock("x1", "COMMENT", "a comment", nil),
		dupLibBlock("x2", "Display.ShowMessageBox", "Show", map[string]string{"Icon": "None", "Message": "hi"}),
		dupLibBlock("x3", "DISABLED_CALL", "Call Helper (disabled)", nil),
		dupLibBlock("x4", "LOOP", "Loop", map[string]string{"Count": "3"},
			dupLibBlock("x5", "COMMENT", "inner", nil),
			dupLibBlock("x6", "WebAutomation.Click.Click", "Click", map[string]string{})),
	}
	other[1].LineNumber, other[3].LineNumber = 40, 41
	got, gotN := subflowBodyHash(&models.Subflow{ID: "other", Name: "Renamed", Blocks: other})
	if got != base || gotN != n {
		t.Errorf("IDs, lines, name, comments, disabled and END blocks changed the hash: %s/%d vs %s/%d", got, gotN, base, n)
	}
	// Pinned (computed independently): the hash is in stored facts and finding
	// metadata, so a change to it needs a FactsVersion bump.
	if base != "e8b2e4f0a6dab411339beae37af928e5" {
		t.Errorf("hash %q changed: bump FactsVersion and re-pin", base)
	}
}

func TestSubflowBodyHash_SeesBehaviour(t *testing.T) {
	show := func(msg string) models.Block {
		return dupLibBlock("s", "Display.ShowMessageBox", "Show", map[string]string{"Message": msg})
	}
	click := dupLibBlock("c", "WebAutomation.Click.Click", "Click", nil)
	loop := func(children ...models.Block) models.Block {
		return dupLibBlock("l", "LOOP", "Loop", nil, children...)
	}
	base, _ := dupLibHash(t, show("hi"), click, loop())
	variants := map[string][]models.Block{
		"property value": {show("bye"), click, loop()},
		"order":          {click, show("hi"), loop()},
		"nesting":        {show("hi"), loop(click)},
		"raw type":       {show("hi"), dupLibBlock("c", "WebAutomation.Click.DoubleClick", "Click", nil), loop()},
		"name":           {show("hi"), dupLibBlock("c", "WebAutomation.Click.Click", "Click it", nil), loop()},
		"property key":   {dupLibBlock("s", "Display.ShowMessageBox", "Show", map[string]string{"Title": "hi"}), click, loop()},
	}
	for name, blocks := range variants {
		if got, _ := dupLibHash(t, blocks...); got == base {
			t.Errorf("%s: the hash did not change", name)
		}
	}
}

func TestDuplicateSubflow_TrivialSubflowsAreIgnored(t *testing.T) {
	two := `#Region "Tiny"
    COMMENT  helper
    SET A TO 1
    SET B TO 2
    DISABLE CALL Other
#EndRegion
`
	three := `#Region "Small"
    SET A TO 1
    SET B TO 2
    SET C TO 3
#EndRegion
`
	extract := func(text string) json.RawMessage {
		data, err := dupLibRule.Extract(libraryRuleContext(libParse(t, "x.txt", text), nil))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if data := extract(two); data != nil {
		t.Errorf("a subflow of 2 counted blocks is trivial, got facts %s", data)
	}
	if data := extract(three); !strings.Contains(string(data), `"name":"Small"`) || !strings.Contains(string(data), `"blocks":3`) {
		t.Errorf("a subflow of 3 counted blocks is not trivial, got facts %s", data)
	}
	docs := []*models.FlowDocument{
		libParse(t, "a.txt", two+three), libParse(t, "b.txt", two+three),
	}
	rep := dupAnalyze(t, docs)
	if len(rep.Findings) != 2 {
		t.Fatalf("got %d findings, want 2 (Small in both flows, never Tiny)", len(rep.Findings))
	}
	for _, f := range rep.Findings {
		if f.SubflowName != "Small" {
			t.Errorf("trivial subflow reported: %+v", f)
		}
	}
}

// dupOtherLogin has a subflow named Login with a body unlike dupTestFlow's.
const dupOtherLogin = `#Region "Login"
    SET Q TO 1
    SET R TO 2
    SET S TO 3
#EndRegion
`

func dupFindingKeys(rep *models.LibraryReport) []string {
	var out []string
	for _, f := range rep.Findings {
		out = append(out, f.FlowName+"/"+f.SubflowName)
	}
	return out
}

func TestDuplicateSubflow_AcrossFlowsOnly(t *testing.T) {
	one := libParse(t, "one.txt", dupTestFlow("Login", "Main")+dupTestFlow("Login2", "Other"))
	two := libParse(t, "two.txt", dupOtherLogin)
	rep := dupAnalyze(t, []*models.FlowDocument{one, two})
	if len(rep.Findings) != 0 {
		t.Fatalf("copies inside one flow only must not be reported: %v", dupFindingKeys(rep))
	}

	three := libParse(t, "three.txt", dupTestFlow("Login", "Main3"))
	rep = dupAnalyze(t, []*models.FlowDocument{three, two, one})
	if got, want := fmt.Sprint(dupFindingKeys(rep)), "[one/Login one/Login2 three/Login]"; got != want {
		t.Fatalf("findings %s, want %s", got, want)
	}
	f := rep.Findings[0]
	if f.ID != "L-001" || f.RuleID != dupLibRule.ID() || f.Severity != models.SeverityInfo || f.Category != "Style" ||
		f.Confidence != models.ConfidenceHigh || f.Stage != string(StagePreview) || f.FlowID != one.ID {
		t.Errorf("stamps: %+v", f)
	}
	if want := "Subflow 'Login' (5 actions) is one of 3 identical copies in 2 flows, including 'Login' in flow 'three'."; f.Description != want {
		t.Errorf("description %q, want %q", f.Description, want)
	}
	if f.Metadata["copies"] != 3 || f.Metadata["flows"] != 2 || f.Metadata["blocks"] != 5 || f.Metadata["subflow"] != "Login" {
		t.Errorf("metadata %v", f.Metadata)
	}
	wantRelated := []models.LibraryLocation{
		{FlowID: one.ID, FlowName: "one", SubflowName: "Login2"},
		{FlowID: three.ID, FlowName: "three", SubflowName: "Login"},
	}
	if fmt.Sprint(f.Related) != fmt.Sprint(wantRelated) {
		t.Errorf("related %v, want %v", f.Related, wantRelated)
	}

	pair := dupAnalyze(t, []*models.FlowDocument{three, libParse(t, "four.txt", dupTestFlow("SignIn", "X"))})
	if want := "Subflow 'Login' (5 actions) is identical to subflow 'SignIn' in flow 'four'."; len(pair.Findings) != 2 ||
		pair.Findings[1].Description != want {
		t.Errorf("pair: %v / %q", dupFindingKeys(pair), pair.Findings[1].Description)
	}
}

func TestDuplicateSubflow_SameNameInOneFlow(t *testing.T) {
	a := libParse(t, "a.txt", dupTestFlow("X", "M1")+dupTestFlow("X", "M2"))
	b := libParse(t, "b.txt", dupTestFlow("X", "M3"))
	rep := dupAnalyze(t, []*models.FlowDocument{a, b})
	if got := fmt.Sprint(dupFindingKeys(rep)); got != "[a/X a/X b/X]" {
		t.Fatalf("findings %s", got)
	}
	if rep.Findings[0].Fingerprint == rep.Findings[1].Fingerprint {
		t.Error("two same-named copies in one flow share a fingerprint")
	}
}

func TestDuplicateSubflow_FingerprintsStableAcrossReparse(t *testing.T) {
	texts := []struct{ path, text string }{
		{"flows/a.txt", dupTestFlow("Login", "Main")},
		{"flows/b.txt", dupTestFlow("Login", "Other")},
		{"flows/c.txt", dupTestFlow("SignIn", "Main")},
	}
	run := func() *models.LibraryReport {
		var docs []*models.FlowDocument
		for _, x := range texts {
			docs = append(docs, libParse(t, x.path, x.text))
		}
		return dupAnalyze(t, docs, LibraryRules()...)
	}
	first, second := run(), run()
	if len(first.Findings) != 5 || len(second.Findings) != len(first.Findings) {
		t.Fatalf("findings %v / %v", dupFindingKeys(first), dupFindingKeys(second))
	}
	for i := range first.Findings {
		a, b := first.Findings[i], second.Findings[i]
		if a.FlowID == b.FlowID {
			t.Fatal("expected fresh document IDs per parse")
		}
		a.FlowID, b.FlowID, a.Related, b.Related = "", "", nil, nil
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Errorf("finding %d changed across re-parse:\n%+v\n%+v", i, a, b)
		}
	}
}

func TestDuplicateSubflow_FactsBudget(t *testing.T) {
	doc := &models.FlowDocument{ID: "big"}
	for i := range 300 {
		var blocks []models.Block
		for k := range 3 + i%7 {
			blocks = append(blocks, dupLibBlock(fmt.Sprint(k), "Variables.SetVariable", fmt.Sprintf("Set %d.%d", i, k), nil))
		}
		doc.Subflows = append(doc.Subflows, models.Subflow{ID: fmt.Sprint(i), Name: fmt.Sprintf("Subflow_with_a_longer_name_%03d", i), Blocks: blocks})
	}
	data, err := dupLibRule.Extract(&RuleContext{Flow: doc})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > MaxLibraryFactsBytes {
		t.Fatalf("facts are %d bytes, over the cap", len(data))
	}
	var facts dupSubflowFacts
	if err := json.Unmarshal(data, &facts); err != nil {
		t.Fatal(err)
	}
	if !facts.Truncated || len(facts.Subflows) == 0 || len(facts.Subflows) == 300 {
		t.Fatalf("truncated %v, kept %d of 300", facts.Truncated, len(facts.Subflows))
	}
	kept := map[string]bool{}
	minKept, prev := 1<<30, -1
	for _, e := range facts.Subflows {
		kept[e.Name] = true
		minKept = min(minKept, e.Blocks)
		var idx int
		if _, err := fmt.Sscanf(e.Name[len(e.Name)-3:], "%d", &idx); err != nil {
			t.Fatal(err)
		}
		if idx <= prev {
			t.Errorf("kept entries are not in document order at %s", e.Name)
		}
		prev = idx
	}
	for i := range 300 {
		if !kept[doc.Subflows[i].Name] && 3+i%7 > minKept {
			t.Errorf("dropped %s (%d blocks) while keeping a %d-block subflow", doc.Subflows[i].Name, 3+i%7, minKept)
		}
	}
}

func TestDuplicateSubflow_RelatedCapped(t *testing.T) {
	var docs []*models.FlowDocument
	for i := range maxLibraryRelated + 10 {
		docs = append(docs, libParse(t, fmt.Sprintf("f%02d.txt", i), dupTestFlow("Login", fmt.Sprintf("Own%d", i))))
	}
	rep := dupAnalyze(t, docs)
	if len(rep.Findings) != len(docs) {
		t.Fatalf("%d findings, want %d", len(rep.Findings), len(docs))
	}
	for _, f := range rep.Findings {
		if len(f.Related) != maxLibraryRelated || f.Metadata["copies"] != len(docs) {
			t.Errorf("%s: %d related, copies %v", f.FlowName, len(f.Related), f.Metadata["copies"])
		}
	}
}

// TestDuplicateSubflow_CorpusCopy: every sample is its own flow, plus a copy
// of one sample under another path; each non-trivial subflow of the copy is
// reported in both flows.
func TestDuplicateSubflow_CorpusCopy(t *testing.T) {
	paths, err := filepath.Glob("../parser/testdata/samples/*.txt")
	if err != nil || len(paths) < 5 {
		t.Fatalf("samples: %v %v", paths, err)
	}
	var docs []*models.FlowDocument
	var copyText string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, libParse(t, p, string(data)))
		if filepath.Base(p) == "complex_flow.txt" {
			copyText = string(data)
		}
	}
	docs = append(docs, libParse(t, "copies/complex_flow_copy.txt", copyText))
	rep := dupAnalyze(t, docs, LibraryRules()...)
	byFlow := map[string][]string{}
	for _, f := range rep.Findings {
		byFlow[f.FlowName] = append(byFlow[f.FlowName], f.SubflowName)
	}
	orig, cp := fmt.Sprint(byFlow["complex_flow"]), fmt.Sprint(byFlow["complex_flow_copy"])
	if len(byFlow["complex_flow"]) == 0 || orig != cp {
		t.Errorf("original reports %s, copy %s", orig, cp)
	}
	want := 0
	copied := docs[len(docs)-1]
	for i := range copied.Subflows {
		if _, n := subflowBodyHash(&copied.Subflows[i]); n >= dupSubflowMinBlocks {
			want++
		}
	}
	if len(byFlow["complex_flow_copy"]) != want {
		t.Errorf("copy reports %d subflows, want every non-trivial one (%d)", len(byFlow["complex_flow_copy"]), want)
	}
}
