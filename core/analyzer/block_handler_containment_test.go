package analyzer

import (
	"os"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// blockWithHandler is PAD's own `BLOCK` idiom: the block opens, ON BLOCK ERROR
// declares the handler, the first END closes the HANDLER, the body follows, and
// the second END closes the BLOCK. Both file operations are protected.
const blockWithHandler = "#Region \"Main\"\n" +
	"BLOCK \n" +
	"ON BLOCK ERROR all\n" +
	"END\n" +
	"    File.Delete Files: $'''C:\\\\temp\\\\x.txt'''\n" +
	"    File.Move Files: $'''C:\\\\a''' Destination: $'''C:\\\\b'''\n" +
	"END\n" +
	"#EndRegion\n"

func rulesFor(t *testing.T, src string) map[string]int {
	t.Helper()
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	rep := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	c := map[string]int{}
	for _, f := range rep.Findings {
		c[f.RuleID]++
	}
	return c
}

// TestBlockErrorHandler_ProtectedBodyIsInsideTheBlock is the regression gate for
// a containment bug that made the analyzer wrong about PAD's commonest
// error-handling construct, in the direction that costs users the most: false
// alarms on correctly written flows.
//
// `BLOCK` + `ON BLOCK ERROR` holds two containers in one promoted node, and PAD
// closes them with two ENDs. handleEnd's skipNextEnd branch popped that node on
// the FIRST end, so the protected body — everything between the two ENDs — was
// emitted at ROOT level as siblings of the block. HasErrorHandlerAncestor walks
// ParentMap, found no entry for a root block, and returned false for every
// action inside a block that demonstrably had a handler.
func TestBlockErrorHandler_ProtectedBodyIsInsideTheBlock(t *testing.T) {
	doc, err := parser.ParseText(blockWithHandler, "Main.txt", int64(len(blockWithHandler)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}

	var roots []string
	for i := range doc.Subflows {
		for j := range doc.Subflows[i].Blocks {
			roots = append(roots, doc.Subflows[i].Blocks[j].RawType)
		}
	}
	if len(roots) != 1 {
		t.Errorf("the protected body escaped the block: %d root blocks %v, want 1 (the block itself)",
			len(roots), roots)
	}

	got := rulesFor(t, blockWithHandler)
	for _, id := range []string{"unhandled-error", "file-op-no-error-handler"} {
		if got[id] != 0 {
			t.Errorf("%s fired %d time(s) on actions inside a block that HAS an error handler",
				id, got[id])
		}
	}
}

// TestBlockErrorHandler_StillReportsGenuinelyUnprotectedWork is the other half.
// Nesting the body inside the handler makes HasErrorHandlerAncestor true for
// everything in it, so the gate above could also be satisfied by a rule that
// never fires. The same actions with no block around them must still be caught.
func TestBlockErrorHandler_StillReportsGenuinelyUnprotectedWork(t *testing.T) {
	bare := "#Region \"Main\"\n" +
		"File.Delete Files: $'''C:\\\\temp\\\\x.txt'''\n" +
		"File.Move Files: $'''C:\\\\a''' Destination: $'''C:\\\\b'''\n" +
		"#EndRegion\n"

	got := rulesFor(t, bare)
	if got["file-op-no-error-handler"] == 0 && got["unhandled-error"] == 0 {
		t.Error("neither rule fired on unprotected file operations — the containment fix " +
			"silenced them everywhere rather than only inside a handled block")
	}
}

// TestEmptyHandler_UnaffectedForThePlainShape pins the rule the containment
// change interacts with.
//
// Scope note, stated because it is a real and deliberate limitation: in the
// PROMOTED shape the handler section and the protected body are children of the
// same node, so an empty handler wrapping a non-empty body is no longer
// distinguishable and is not reported. Separating them needs a distinct block
// type for the handler section, which is a larger change than this one. The
// plain `ON ERROR ... END` form — the shape the rule actually targets — is
// unaffected, and that is what this test holds.
func TestEmptyHandler_UnaffectedForThePlainShape(t *testing.T) {
	empty := "#Region \"Main\"\nFile.Delete Files: $'''C:\\\\t\\\\x.txt'''\nON ERROR\nEND\n#EndRegion\n"
	if got := rulesFor(t, empty)["empty-handler"]; got != 1 {
		t.Errorf("empty-handler fired %d time(s) on an empty ON ERROR handler, want 1", got)
	}

	filled := "#Region \"Main\"\nFile.Delete Files: $'''C:\\\\t\\\\x.txt'''\nON ERROR\n" +
		"    Display.ShowMessageBox Message: $'''failed'''\nEND\n#EndRegion\n"
	if got := rulesFor(t, filled)["empty-handler"]; got != 0 {
		t.Errorf("empty-handler fired %d time(s) on a handler that has a body, want 0", got)
	}
}

// TestBlockErrorHandler_RealPADSample holds the fix against Microsoft's own
// published flow rather than a fixture written to suit it. 33 of its 42 lines
// are the protected body; before the fix every one of them was a root sibling.
func TestBlockErrorHandler_RealPADSample(t *testing.T) {
	b, err := os.ReadFile("../parser/testdata/samples/browsercloser/Main.txt")
	if err != nil {
		t.Skipf("sample unavailable: %v", err)
	}
	src := string(b)
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}

	var handler *models.Block
	for i := range doc.Subflows {
		for j := range doc.Subflows[i].Blocks {
			if doc.Subflows[i].Blocks[j].RawType == "OnBlockError" {
				handler = &doc.Subflows[i].Blocks[j]
			}
		}
	}
	if handler == nil {
		t.Fatal("no promoted BLOCK in the sample — fixture rot")
	}
	// Marker + handler END + the body. Before the fix this was exactly 2.
	if len(handler.Children) <= 2 {
		t.Errorf("the promoted block has %d children — the protected body is still outside it",
			len(handler.Children))
	}
}
