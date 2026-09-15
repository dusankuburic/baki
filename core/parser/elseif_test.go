package parser

import (
	"strings"
	"testing"

	"pad-core/models"
)

const elseIfFlow = "#Region \"Main\"\n" +
	"IF %Status% = 'new' THEN\n" +
	"    Web.Post Url: $'''https://a'''\n" +
	"ELSE IF %Status% = 'admin' THEN\n" +
	"    System.RunApplication ApplicationPath: $'''cmd.exe''' CommandLineArguments: %UserInput%\n" +
	"ELSE\n" +
	"    Display.ShowMessageBox Message: $'''no'''\n" +
	"END\n" +
	"#EndRegion\n"

// findBlock walks the whole tree for the first block whose RawType matches.
func findBlock(doc *models.FlowDocument, rawType string) *models.Block {
	var found *models.Block
	var walk func(bs []models.Block)
	walk = func(bs []models.Block) {
		for i := range bs {
			if found != nil {
				return
			}
			if strings.EqualFold(bs[i].RawType, rawType) {
				found = &bs[i]
				return
			}
			walk(bs[i].Children)
		}
	}
	for i := range doc.Subflows {
		walk(doc.Subflows[i].Blocks)
	}
	return found
}

// TestElseIf_BranchIsNotReparentedIntoTheThenBranch is the regression gate for a
// silent mis-parse of one of PAD's commonest constructs.
//
// reIfStart is anchored on a leading IF and reElse is anchored with \s*$, so
// `ELSE IF %x% = 'admin' THEN` matched NEITHER. It fell through classifyLine to
// the UNKNOWN branch, which records a warning-level "malformed line" and creates
// NO block at all. The actions that followed were then attached by indent to the
// still-open THEN branch — so an action reachable only when Status = 'admin' was
// reported, to every rule and to the user, as part of the Status = 'new' path.
// The branch condition was never a block either, so %Status% was never extracted
// on that rung.
func TestElseIf_BranchIsNotReparentedIntoTheThenBranch(t *testing.T) {
	doc, err := ParseText(elseIfFlow, "Main.txt", int64(len(elseIfFlow)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}

	for _, pe := range doc.ParseErrors {
		if strings.Contains(pe.Message, "malformed") {
			t.Errorf("ELSE IF is still unclassifiable: %+v", pe)
		}
	}

	elseIf := findBlock(doc, "ELSE IF")
	if elseIf == nil {
		t.Fatal("no ELSE IF block in the tree — the rung produced no block")
	}

	// The admin-only action must hang off the admin rung, not the THEN branch.
	var childRawTypes []string
	for i := range elseIf.Children {
		childRawTypes = append(childRawTypes, elseIf.Children[i].RawType)
	}
	if len(childRawTypes) != 1 || childRawTypes[0] != "System.RunApplication" {
		t.Errorf("ELSE IF children = %v, want exactly [System.RunApplication] — "+
			"an action gated on one branch is attributed to another", childRawTypes)
	}

	ifBlock := findBlock(doc, "IF")
	for i := range ifBlock.Children {
		if ifBlock.Children[i].RawType == "System.RunApplication" {
			t.Error("the admin-branch action is a direct child of the THEN branch")
		}
	}

	// The rung's own condition must be extracted, or no rule can reason about it.
	if len(elseIf.Variables) == 0 {
		t.Errorf("ELSE IF extracted no variables from %q — the branch condition is invisible", elseIf.Name)
	}
}

// TestElseIf_RoundTrips is the half that protects the file itself. BlockTypeElse
// previously serialized as the bare keyword "ELSE"; had the rung been given that
// type without teaching the serializer about it, every save would have silently
// dropped the condition and merged the admin branch into the final else.
func TestElseIf_RoundTrips(t *testing.T) {
	doc, err := ParseText(elseIfFlow, "Main.txt", int64(len(elseIfFlow)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	out := SerializeDocument(doc)

	// The serializer is deliberately not byte-lossless (see serialize.go's
	// header): it normalises quoting and drops a trailing THEN whose keyword the
	// tokenizer stripped — an `IF ... THEN` re-emits the same way. What must
	// survive is the CONDITION, without which the rung collapses into the final
	// else.
	if !strings.Contains(out, "ELSE IF %Status% = 'admin'") {
		t.Errorf("the branch condition did not survive serialization:\n%s", out)
	}
	// Re-parse must yield the same shape, or the second save diverges again.
	doc2, err := ParseText(out, "Main.txt", int64(len(out)))
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	elseIf2 := findBlock(doc2, "ELSE IF")
	if elseIf2 == nil {
		t.Fatalf("ELSE IF vanished on re-parse:\n%s", out)
	}
	if len(elseIf2.Children) != 1 || elseIf2.Children[0].RawType != "System.RunApplication" {
		t.Errorf("branch contents changed across a round trip:\n%s", out)
	}
	// Exactly one END for the whole chain — an extra END would close the
	// enclosing construct early on every subsequent parse.
	if got := strings.Count(out, "END"); got != 1 {
		t.Errorf("serialized %d ENDs for one IF chain, want 1:\n%s", got, out)
	}
}

// TestPlainElse_Unchanged guards the refactor of handleConditionElse onto
// newBlock: a bare ELSE must still produce the block it always did.
func TestPlainElse_Unchanged(t *testing.T) {
	src := "#Region \"Main\"\nIF %A% = 1 THEN\n    Web.Post Url: $'''https://a'''\n" +
		"ELSE\n    Display.ShowMessageBox Message: $'''no'''\nEND\n#EndRegion\n"
	doc, err := ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	el := findBlock(doc, "ELSE")
	if el == nil {
		t.Fatal("plain ELSE produced no block")
	}
	if el.Name != "Else" || el.Type != models.BlockTypeElse {
		t.Errorf("plain ELSE changed shape: Name=%q Type=%v", el.Name, el.Type)
	}
	out := SerializeDocument(doc)
	var sawBareElse bool
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) == "ELSE" {
			sawBareElse = true
		}
	}
	if !sawBareElse {
		t.Errorf("plain ELSE no longer serializes as the bare keyword:\n%s", out)
	}
}
