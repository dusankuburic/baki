package parser

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-core/models"
)

var update = flag.Bool("update", false, "update golden files")

type normalizedDoc struct {
	Name        string              `json:"name"`
	Subflows    []normalizedSF      `json:"subflows"`
	ParseErrors []models.ParseError `json:"parseErrors,omitempty"`
	Metadata    struct {
		BlockCount   int `json:"blockCount"`
		SubflowCount int `json:"subflowCount"`
		MaxDepth     int `json:"maxDepth"`
		RawLineCount int `json:"rawLineCount"`
	} `json:"metadata"`
}

type normalizedSF struct {
	Name   string          `json:"name"`
	Blocks []normalizedBlk `json:"blocks"`
}

type normalizedBlk struct {
	Name       string              `json:"name"`
	Type       models.BlockType    `json:"type"`
	RawType    string              `json:"rawType"`
	Indent     int                 `json:"indent"`
	LineNumber int                 `json:"lineNumber"`
	Properties map[string]string   `json:"properties"`
	Variables  []string            `json:"variables"`
	Tokens     []models.BlockToken `json:"tokens,omitempty"`
	Children   []normalizedBlk     `json:"children"`
}

func normalizeDoc(doc *models.FlowDocument) normalizedDoc {
	nd := normalizedDoc{
		Name:        doc.Name,
		ParseErrors: doc.ParseErrors,
	}
	nd.Metadata.BlockCount = doc.Metadata.BlockCount
	nd.Metadata.SubflowCount = doc.Metadata.SubflowCount
	nd.Metadata.MaxDepth = doc.Metadata.MaxDepth
	nd.Metadata.RawLineCount = doc.Metadata.RawLineCount

	for _, sf := range doc.Subflows {
		nd.Subflows = append(nd.Subflows, normalizedSF{
			Name:   sf.Name,
			Blocks: normalizeBlocks(sf.Blocks),
		})
	}
	return nd
}

func normalizeBlocks(blocks []models.Block) []normalizedBlk {
	result := make([]normalizedBlk, len(blocks))
	for i, b := range blocks {
		result[i] = normalizedBlk{
			Name:       b.Name,
			Type:       b.Type,
			RawType:    b.RawType,
			Indent:     b.Indent,
			LineNumber: b.LineNumber,
			Properties: b.Properties,
			Variables:  b.Variables,
			Tokens:     b.Tokens,
			Children:   normalizeBlocks(b.Children),
		}
	}
	return result
}

func TestParser_MinimalFlow(t *testing.T) {
	input := `#Region "Main"
    Display.ShowMessageBox Message: $'''Simple flow'''
#EndRegion`

	doc, err := ParseText(input, "minimal.txt", int64(len(input)))
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}

	if len(doc.Subflows) != 1 {
		t.Fatalf("expected 1 subflow, got %d", len(doc.Subflows))
	}
	if doc.Subflows[0].Name != "Main" {
		t.Errorf("expected subflow name 'Main', got %q", doc.Subflows[0].Name)
	}
	if len(doc.Subflows[0].Blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(doc.Subflows[0].Blocks))
	}
	blk := doc.Subflows[0].Blocks[0]
	if blk.Type != models.BlockTypeAction {
		t.Errorf("expected ACTION type, got %q", blk.Type)
	}
	if blk.RawType != "Display.ShowMessageBox" {
		t.Errorf("expected rawType Display.ShowMessageBox, got %q", blk.RawType)
	}
	if blk.Properties["Message"] != "Simple flow" {
		t.Errorf("expected Message='Simple flow', got %q", blk.Properties["Message"])
	}
}

func TestParser_LoopAndCondition(t *testing.T) {
	input := `#Region "Main"
    SET Counter TO 0
    LOOP WHILE %Counter% < 5
        IF %Counter% > 2
            Display.ShowMessageBox Message: %Counter%
        ELSE
            Variables.IncreaseVariable Value: 1 Name: Counter
        END
    END
#EndRegion`

	doc, err := ParseText(input, "loop_cond.txt", int64(len(input)))
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}

	if len(doc.Subflows) != 1 {
		t.Fatalf("expected 1 subflow, got %d", len(doc.Subflows))
	}

	sf := doc.Subflows[0]
	if len(sf.Blocks) < 2 {
		t.Fatalf("expected at least 2 root blocks (SET + LOOP), got %d", len(sf.Blocks))
	}

	if sf.Blocks[0].Type != models.BlockTypeVariable {
		t.Errorf("expected first block VARIABLE, got %q", sf.Blocks[0].Type)
	}

	loop := sf.Blocks[1]
	if loop.Type != models.BlockTypeLoop {
		t.Fatalf("expected second block LOOP, got %q", loop.Type)
	}

	if len(loop.Children) == 0 {
		t.Fatal("loop has no children")
	}

	cond := loop.Children[0]
	if cond.Type != models.BlockTypeCondition {
		t.Errorf("expected CONDITION inside loop, got %q", cond.Type)
	}

	if doc.Metadata.BlockCount == 0 {
		t.Error("expected BlockCount > 0")
	}
	if doc.Metadata.SubflowCount != 1 {
		t.Errorf("expected SubflowCount=1, got %d", doc.Metadata.SubflowCount)
	}
}

func TestParser_MultipleSubflows(t *testing.T) {
	input := `#Region "Main"
    COMMENT  First subflow
    Display.ShowMessageBox Message: $'''hello'''
#EndRegion
#Region "Helper"
    COMMENT  Second subflow
    SET Result TO $'''done'''
#EndRegion`

	doc, err := ParseText(input, "multi.txt", int64(len(input)))
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}

	if len(doc.Subflows) != 2 {
		t.Fatalf("expected 2 subflows, got %d", len(doc.Subflows))
	}
	if doc.Subflows[0].Name != "Main" {
		t.Errorf("expected first subflow 'Main', got %q", doc.Subflows[0].Name)
	}
	if doc.Subflows[1].Name != "Helper" {
		t.Errorf("expected second subflow 'Helper', got %q", doc.Subflows[1].Name)
	}
}

func TestParser_ErrorHandler(t *testing.T) {
	input := `#Region "Main"
    ON BLOCK ERROR
        Display.ShowMessageBox Message: $'''error'''
    END
    COMMENT  Continue
#EndRegion`

	doc, err := ParseText(input, "error.txt", int64(len(input)))
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}

	sf := doc.Subflows[0]
	if len(sf.Blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(sf.Blocks))
	}

	eh := sf.Blocks[0]
	if eh.Type != models.BlockTypeErrorHandler {
		t.Errorf("expected ERROR_HANDLER, got %q", eh.Type)
	}
	if len(eh.Children) != 2 {
		t.Errorf("expected 2 children in error handler (Action + End), got %d", len(eh.Children))
	}
}

func TestParser_Comments(t *testing.T) {
	input := `#Region "Main"
    COMMENT  This is a comment
    COMMENT  Another comment
#EndRegion`

	doc, err := ParseText(input, "comments.txt", int64(len(input)))
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}

	sf := doc.Subflows[0]
	if len(sf.Blocks) != 2 {
		t.Fatalf("expected 2 comment blocks, got %d", len(sf.Blocks))
	}
	for i, b := range sf.Blocks {
		if b.Type != models.BlockTypeComment {
			t.Errorf("block %d: expected COMMENT, got %q", i, b.Type)
		}
	}
}

func TestParser_Variables(t *testing.T) {
	input := `#Region "Main"
    SET MyVar TO $'''hello'''
    Variables.IncreaseVariable Value: 1 Name: Counter
    Variables.DecreaseVariable Value: 1 Name: Counter
#EndRegion`

	doc, err := ParseText(input, "vars.txt", int64(len(input)))
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}

	sf := doc.Subflows[0]
	for _, b := range sf.Blocks {
		if b.Type != models.BlockTypeVariable && b.Type != models.BlockTypeAction {
			t.Errorf("expected VARIABLE or ACTION, got %q (rawType=%q)", b.Type, b.RawType)
		}
	}
}

func TestParser_PropertiesWithVariables(t *testing.T) {
	input := `#Region "Main"
    WebAutomation.NavigateTo Url: $'''https://example.com?user=%Username%''' BrowserInstance: %Browser%
#EndRegion`

	doc, err := ParseText(input, "props.txt", int64(len(input)))
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}

	blk := doc.Subflows[0].Blocks[0]
	vars := blk.Variables
	foundUser := false
	foundBrowser := false
	for _, v := range vars {
		if v == "Username" {
			foundUser = true
		}
		if v == "Browser" {
			foundBrowser = true
		}
	}
	if !foundUser {
		t.Error("expected variable 'Username' to be extracted")
	}
	if !foundBrowser {
		t.Error("expected variable 'Browser' to be extracted")
	}
}

func TestParser_Metadata(t *testing.T) {
	input := `#Region "Main"
    COMMENT  test
    Display.ShowMessageBox Message: $'''hi'''
#EndRegion`

	doc, err := ParseText(input, "meta.txt", int64(len(input)))
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}

	if doc.Name != "meta" {
		t.Errorf("expected doc name 'meta', got %q", doc.Name)
	}
	if doc.Metadata.RawLineCount == 0 {
		t.Error("expected RawLineCount > 0")
	}
	if doc.Metadata.FileSize == 0 {
		t.Error("expected FileSize > 0")
	}
	if doc.Metadata.ParsedAt.IsZero() {
		t.Error("expected ParsedAt to be set")
	}
}

func TestParser_EmptyInput(t *testing.T) {
	doc, err := ParseText("", "empty.txt", 0)
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}
	if len(doc.Subflows) != 1 {
		t.Errorf("expected 1 implicit subflow for empty input, got %d", len(doc.Subflows))
	}
}

func TestParser_GoldenFiles(t *testing.T) {
	samplesDir := "testdata/samples"
	entries, err := os.ReadDir(samplesDir)
	if err != nil {
		t.Skipf("testdata/samples not found: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			name := entry.Name()
			t.Run(name, func(t *testing.T) {
				doc, err := ParseFolder(filepath.Join(samplesDir, name))
				if err != nil {
					t.Fatalf("ParseFolder failed: %v", err)
				}

				nd := normalizeDoc(doc)

				goldenPath := filepath.Join("testdata", "expected", name+".json")

				if *update {
					data, err := json.MarshalIndent(nd, "", "  ")
					if err != nil {
						t.Fatalf("marshal: %v", err)
					}
					os.MkdirAll(filepath.Dir(goldenPath), 0o755)
					os.WriteFile(goldenPath, data, 0o644)
					t.Logf("updated golden file: %s", goldenPath)
					return
				}

				goldenBytes, err := os.ReadFile(goldenPath)
				if err != nil {
					t.Fatalf("read golden file %s: %v (run with -update to create)", goldenPath, err)
				}

				actual, err := json.MarshalIndent(nd, "", "  ")
				if err != nil {
					t.Fatalf("marshal actual: %v", err)
				}

				if string(goldenBytes) != string(actual) {
					t.Errorf("output does not match golden file %s", goldenPath)
					abbreviated := string(actual)
					if len(abbreviated) > 500 {
						abbreviated = abbreviated[:500] + "..."
					}
					t.Logf("actual:\n%s", abbreviated)
				}
			})
			continue
		}
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			inputBytes, err := os.ReadFile(filepath.Join(samplesDir, name))
			if err != nil {
				t.Fatalf("read sample: %v", err)
			}

			input := string(inputBytes)
			doc, err := ParseText(input, name, int64(len(input)))
			if err != nil {
				t.Fatalf("ParseText failed: %v", err)
			}

			nd := normalizeDoc(doc)

			goldenPath := filepath.Join("testdata", "expected", strings.TrimSuffix(name, filepath.Ext(name))+".json")

			if *update {
				data, err := json.MarshalIndent(nd, "", "  ")
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				os.MkdirAll(filepath.Dir(goldenPath), 0o755)
				os.WriteFile(goldenPath, data, 0o644)
				t.Logf("updated golden file: %s", goldenPath)
				return
			}

			goldenBytes, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden file %s: %v (run with -update to create)", goldenPath, err)
			}

			actual, err := json.MarshalIndent(nd, "", "  ")
			if err != nil {
				t.Fatalf("marshal actual: %v", err)
			}

			if string(goldenBytes) != string(actual) {
				t.Errorf("output does not match golden file %s", goldenPath)
				abbreviated := string(actual)
				if len(abbreviated) > 500 {
					abbreviated = abbreviated[:500] + "..."
				}
				t.Logf("actual:\n%s", abbreviated)
			}
		})
	}
}

func TestParser_Switch(t *testing.T) {
	input := `#Region "Main"
    SWITCH %Status%
        CASE 'Active'
            Display.ShowMessageBox Message: $'''Active'''
        CASE 'Inactive'
            Display.ShowMessageBox Message: $'''Inactive'''
        DEFAULT
            Display.ShowMessageBox Message: $'''Unknown'''
    END
#EndRegion`

	doc, err := ParseText(input, "switch.txt", int64(len(input)))
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}

	sf := doc.Subflows[0]
	if len(sf.Blocks) == 0 {
		t.Fatal("expected blocks in subflow")
	}

	switchBlk := sf.Blocks[0]
	if switchBlk.Type != models.BlockTypeSwitch {
		t.Errorf("expected first block to be SWITCH, got %q", switchBlk.Type)
	}

	// SWITCH should have CASE and DEFAULT children
	caseCount := 0
	defaultCount := 0
	for _, child := range switchBlk.Children {
		switch child.Type {
		case models.BlockTypeCase:
			caseCount++
		case models.BlockTypeDefault:
			defaultCount++
		}
	}
	if caseCount != 2 {
		t.Errorf("expected 2 CASE blocks, got %d", caseCount)
	}
	if defaultCount != 1 {
		t.Errorf("expected 1 DEFAULT block, got %d", defaultCount)
	}
}

func TestParser_GotoLabel(t *testing.T) {
	input := `#Region "Main"
    GOTO 'Done'
    Display.ShowMessageBox Message: $'''skipped'''
    LABEL 'Done'
    Display.ShowMessageBox Message: $'''finished'''
#EndRegion`

	doc, err := ParseText(input, "goto.txt", int64(len(input)))
	if err != nil {
		t.Fatalf("ParseText failed: %v", err)
	}

	sf := doc.Subflows[0]
	var gotoBlock, labelBlock *models.Block
	for i := range sf.Blocks {
		b := &sf.Blocks[i]
		if b.RawType == "GOTO" {
			gotoBlock = b
		}
		if b.RawType == "LABEL" {
			labelBlock = b
		}
	}

	if gotoBlock == nil {
		t.Fatal("expected a GOTO block")
	}
	if labelBlock == nil {
		t.Fatal("expected a LABEL block")
	}
	if gotoBlock.Type != models.BlockTypeAction {
		t.Errorf("GOTO block type = %q, want ACTION", gotoBlock.Type)
	}
	if labelBlock.Type != models.BlockTypeAction {
		t.Errorf("LABEL block type = %q, want ACTION", labelBlock.Type)
	}
	// Name should be the target label without quotes
	if gotoBlock.Name != "Done" {
		t.Errorf("GOTO block Name = %q, want %q", gotoBlock.Name, "Done")
	}
	if labelBlock.Name != "Done" {
		t.Errorf("LABEL block Name = %q, want %q", labelBlock.Name, "Done")
	}
}

func TestParser_NoPanicOnGarbage(t *testing.T) {
	garbageInputs := []string{
		"!!!@#$%^&*()",
		"\x00\x01\x02\x03",
		strings.Repeat("A", 100000),
		"#Region\n" + strings.Repeat("    LOOP\n", 300) + strings.Repeat("    END\n", 300) + "#EndRegion",
		"#Region \"A\"\n#Region \"B\"\n#EndRegion\n#EndRegion",
	}

	for i, input := range garbageInputs {
		t.Run(fmt.Sprintf("garbage_%d", i), func(t *testing.T) {
			doc, err := ParseText(input, "garbage.txt", int64(len(input)))
			if err != nil {
				t.Logf("error (expected for garbage): %v", err)
			}
			if doc == nil {
				t.Error("expected non-nil document even for garbage input")
			}
		})
	}
}

func FuzzParser(f *testing.F) {
	seeds := []string{
		"#Region \"Main\"\n    COMMENT  test\n#EndRegion",
		"#Region \"A\"\n    LOOP FOREACH X IN %L%\n        IF %X% > 0\n            SET Y TO %X%\n        END\n    END\n#EndRegion",
		"garbage input\n!!!\n\x00\x01",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		doc, err := ParseText(input, "fuzz.txt", int64(len(input)))
		if err != nil {
			t.Logf("error: %v", err)
		}
		if doc == nil {
			t.Error("expected non-nil document")
		}
	})
}

func BenchmarkParser10kLines(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("#Region \"Main\"\n")
	for i := 0; i < 10000; i++ {
		switch i % 5 {
		case 0:
			sb.WriteString("    Display.ShowMessageBox Message: $'''Message %Counter%'''\n")
		case 1:
			sb.WriteString("    Variables.SetVariable NewValue: %Value% Name: Var\n")
		case 2:
			sb.WriteString("    COMMENT  This is comment line\n")
		case 3:
			sb.WriteString("    DateTime.GetCurrentDateTime DateTimeFormat: DateTime.DateTimeFormat.DateAndTime => CurrentDT\n")
		case 4:
			sb.WriteString("    WebAutomation.Click.Click BrowserInstance: %Browser% Control: appmask['Button']\n")
		}
	}
	sb.WriteString("#EndRegion")

	input := sb.String()
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := ParseText(input, "bench.txt", int64(len(input)))
		if err != nil {
			b.Fatal(err)
		}
	}
}

// ParseFiles must assign a StableID derived from the file-name set (not
// content): session analytics key on it so re-uploading an edited file updates
// one dashboard entry instead of adding a phantom flow per parse.
func TestParseFiles_StableIDSurvivesReparse(t *testing.T) {
	files := map[string]string{"Main.txt": "SET x TO 1\n"}
	doc1, err := ParseFiles(files, "upload")
	if err != nil {
		t.Fatal(err)
	}
	edited := map[string]string{"Main.txt": "SET x TO 2\nSET y TO 3\n"}
	doc2, err := ParseFiles(edited, "upload")
	if err != nil {
		t.Fatal(err)
	}
	if doc1.StableID == "" {
		t.Fatal("ParseFiles must set StableID")
	}
	if doc1.StableID != doc2.StableID {
		t.Errorf("same file names must give the same StableID across parses: %s vs %s", doc1.StableID, doc2.StableID)
	}
	if doc1.ID == doc2.ID {
		t.Error("doc.ID should still be fresh per parse")
	}

	other, err := ParseFiles(map[string]string{"Other.txt": "SET x TO 1\n"}, "upload")
	if err != nil {
		t.Fatal(err)
	}
	if other.StableID == doc1.StableID {
		t.Error("different file names must give different StableIDs")
	}
}
