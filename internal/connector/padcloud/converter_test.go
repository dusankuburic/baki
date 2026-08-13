package padcloud

import (
	"encoding/json"
	"testing"

	"pad-core/models"
)

// fixture is a representative PAD-cloud action tree shaped after the documented
// schema (nested "actions" with "type"+"properties", plus a LOOP container).
const fixture = `{
  "name": "Invoice Processing",
  "actions": [
    {
      "type": "Excel.LaunchExcel",
      "comment": "Open workbook",
      "properties": {"Document": "%WorkbookPath%", "Visible": true},
      "actions": [
        {"type": "Excel.ReadCell", "properties": {"Cell": "A1", "Value": "%CellValue%"}}
      ]
    },
    {
      "type": "LOOP",
      "actions": [
        {"type": "Variables.IncreaseVariable", "properties": {"Counter": "%i%"}}
      ]
    }
  ]
}`

func TestCloudConverter_BuildsDocument(t *testing.T) {
	c := NewCloudConverter()
	doc, err := c.Convert("flow.txt", json.RawMessage(fixture))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if doc == nil {
		t.Fatal("Convert returned nil document")
	}

	if doc.Name != "Invoice Processing" {
		t.Errorf("Name = %q, want %q (definition name wins over file name)", doc.Name, "Invoice Processing")
	}
	if len(doc.Subflows) != 1 {
		t.Fatalf("Subflows = %d, want 1", len(doc.Subflows))
	}
	sf := doc.Subflows[0]
	if len(sf.Blocks) != 2 {
		t.Fatalf("top-level blocks = %d, want 2", len(sf.Blocks))
	}

	launch := sf.Blocks[0]
	if launch.RawType != "Excel.LaunchExcel" {
		t.Errorf("first block RawType = %q, want Excel.LaunchExcel", launch.RawType)
	}
	if launch.Name != "Open workbook" {
		t.Errorf("first block Name = %q, want comment %q", launch.Name, "Open workbook")
	}
	if launch.Type != "ACTION" {
		t.Errorf("first block Type = %q, want ACTION", launch.Type)
	}
	if len(launch.Children) != 1 {
		t.Fatalf("first block children = %d, want 1 (nested ReadCell)", len(launch.Children))
	}
	if launch.Children[0].RawType != "Excel.ReadCell" {
		t.Errorf("nested child RawType = %q, want Excel.ReadCell", launch.Children[0].RawType)
	}
	if launch.Children[0].ParentID != launch.ID {
		t.Error("nested child ParentID not set to parent block id")
	}

	// Properties flattened to strings, including a bool → "true".
	if launch.Properties["Document"] != "%WorkbookPath%" {
		t.Errorf("Document prop = %q, want %%WorkbookPath%%", launch.Properties["Document"])
	}
	if launch.Properties["Visible"] != "true" {
		t.Errorf("Visible prop = %q, want true (bool stringified)", launch.Properties["Visible"])
	}

	// All four blocks counted (Launch, ReadCell, Loop, Increase).
	if doc.Metadata.BlockCount != 4 {
		t.Errorf("BlockCount = %d, want 4", doc.Metadata.BlockCount)
	}
	if doc.Metadata.MaxDepth != 2 {
		t.Errorf("MaxDepth = %d, want 2", doc.Metadata.MaxDepth)
	}

	// RebuildIndexes ran (BlocksByID populated, every block reachable).
	if len(doc.BlocksByID) != 4 {
		t.Errorf("BlocksByID = %d entries, want 4 (RebuildIndexes)", len(doc.BlocksByID))
	}
	for _, b := range doc.BlocksByID {
		if b.SubflowID != sf.ID {
			t.Error("a block's SubflowID does not match the subflow")
		}
	}
}

func TestCloudConverter_ExtractsVariables(t *testing.T) {
	c := NewCloudConverter()
	doc, _ := c.Convert("f", json.RawMessage(fixture))

	want := map[string]bool{"WorkbookPath": true, "CellValue": true, "i": true}
	got := map[string]bool{}
	for _, sf := range doc.Subflows {
		for i := range sf.Blocks {
			collectVars(&sf.Blocks[i], got)
		}
	}
	for v := range want {
		if !got[v] {
			t.Errorf("variable %q not extracted from properties", v)
		}
	}
}

// Numeric properties must stringify without scientific notation: JSON numbers
// decode to float64, and fmt.Sprint would render 1000000 as "1e+06", corrupting
// the value the analyzer reads.
func TestCloudConverter_NumericPropertiesNoScientificNotation(t *testing.T) {
	const doc = `{
	  "name": "Numbers",
	  "actions": [
	    {"type": "Wait", "properties": {"Timeout": 1000000, "Port": 8080, "Big": 12345678, "Ratio": 1.5, "Zero": 0}}
	  ]
	}`
	c := NewCloudConverter()
	got, err := c.Convert("f", json.RawMessage(doc))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	props := got.Subflows[0].Blocks[0].Properties
	for k, want := range map[string]string{
		"Timeout": "1000000",
		"Port":    "8080",
		"Big":     "12345678",
		"Ratio":   "1.5",
		"Zero":    "0",
	} {
		if props[k] != want {
			t.Errorf("property %q = %q, want %q", k, props[k], want)
		}
	}
}

// collectVars walks a block tree, accumulating %var% names extracted by the
// converter into out.
func collectVars(b *models.Block, out map[string]bool) {
	for _, v := range b.Variables {
		out[v] = true
	}
	for i := range b.Children {
		collectVars(&b.Children[i], out)
	}
}

func TestCloudConverter_TypeAliases(t *testing.T) {
	// "typeName" and "actionType" should be accepted when "type" is absent, and
	// a node with no type at all falls back to an empty rawType (ACTION).
	c := NewCloudConverter()
	def := `{"actions":[
		{"typeName": "HTTP.Get", "properties": {"Url": "https://x"}},
		{"actionType": "Database.Connect"},
		{"comment": "bare node"}
	]}`
	doc, err := c.Convert("f", json.RawMessage(def))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if doc == nil {
		t.Fatal("expected a document")
	}
	sf := doc.Subflows[0]
	if sf.Blocks[0].RawType != "HTTP.Get" {
		t.Errorf("block0 RawType = %q, want HTTP.Get (typeName alias)", sf.Blocks[0].RawType)
	}
	if sf.Blocks[1].RawType != "Database.Connect" {
		t.Errorf("block1 RawType = %q, want Database.Connect (actionType alias)", sf.Blocks[1].RawType)
	}
	if sf.Blocks[2].Name != "bare node" {
		t.Errorf("block2 Name = %q, want bare node (comment)", sf.Blocks[2].Name)
	}
}

func TestCloudConverter_EmptyDefinition_ReturnsNil(t *testing.T) {
	c := NewCloudConverter()
	doc, err := c.Convert("f", json.RawMessage(`{"actions":[]}`))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if doc != nil {
		t.Errorf("expected nil document for empty action tree, got %+v", doc)
	}
}

func TestCloudConverter_UnwrapsDefinitionEnvelope(t *testing.T) {
	// Some responses wrap the action tree under "definition" (and even twice).
	wrapped := `{"definition": {"definition": ` + fixture + `}}`
	c := NewCloudConverter()
	doc, err := c.Convert("f", json.RawMessage(wrapped))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if doc == nil || len(doc.Subflows[0].Blocks) != 2 {
		t.Fatal("envelope unwrap failed: expected 2 top-level blocks")
	}
}

func TestCloudConverter_InvalidJSON_ReturnsError(t *testing.T) {
	c := NewCloudConverter()
	if _, err := c.Convert("f", json.RawMessage(`{not json`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// Subscripted (%Items[0]%) and dotted (%Item.Prop%) references must count as
// uses of the base variable — the old bare-identifier regex dropped them, so
// the analyzer under-counted variables on any flow indexing into lists/objects.
func TestCloudConverter_ExtractsSubscriptedAndDottedVariables(t *testing.T) {
	const doc = `{
	  "name": "Refs",
	  "actions": [
	    {"type": "Loop", "properties": {
	      "First":  "%Items[0]%",
	      "Prop":   "%Item.Prop%",
	      "Keyed":  "%Row['Name']%",
	      "Plain":  "%Counter%",
	      "Pair":   "%A.x% then %B%"
	    }}
	  ]
	}`
	c := NewCloudConverter()
	got, err := c.Convert("f", json.RawMessage(doc))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	vars := map[string]bool{}
	for _, v := range got.Subflows[0].Blocks[0].Variables {
		vars[v] = true
	}
	for _, want := range []string{"Items", "Item", "Row", "Counter", "A", "B"} {
		if !vars[want] {
			t.Errorf("variable %q not extracted (got %v)", want, vars)
		}
	}
}

// TestCloudConverter_DeepNestingDoesNotOverflow verifies toBlock's depth cap: a
// cloud action tree nested beyond maxConvertDepth must truncate instead of
// recursing until the goroutine stack overflows. A hostile ~50k-level Dataverse
// response is only ~3 MB but overflows a 1 GB stack via unbounded recursion,
// which (before the per-flow recover) took down the whole ingest sweep.
func TestCloudConverter_DeepNestingDoesNotOverflow(t *testing.T) {
	depth := maxConvertDepth + 200
	node := cloudAction{Type: "Display.ShowMessageBox"}
	for i := 0; i < depth; i++ {
		node = cloudAction{Type: "Group", Actions: []cloudAction{node}}
	}
	def := cloudDefinition{Name: "Deep", Actions: []cloudAction{node}}
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal deep tree: %v", err)
	}

	c := NewCloudConverter()
	// Must not panic / overflow; the tree is truncated at the depth cap.
	doc, err := c.Convert("Deep.txt", raw)
	if err != nil {
		t.Fatalf("convert deep tree: %v", err)
	}
	if doc == nil {
		t.Fatal("expected a document, got nil")
	}
}
