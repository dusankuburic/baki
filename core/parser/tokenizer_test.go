package parser

import (
	"testing"
)

func TestTokenize_EmptyInput(t *testing.T) {
	tokens := Tokenize("")
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens, got %d", len(tokens))
	}
}

func TestTokenize_SingleSubflow(t *testing.T) {
	input := `#Region "Main"
    COMMENT  Hello world
    Display.ShowMessageBox Message: $'''Welcome'''
#EndRegion`

	tokens := Tokenize(input)

	expected := []struct {
		kind    TokenKind
		name    string
		rawType string
	}{
		{TokSubflowStart, "Main", "Region"},
		{TokComment, "Hello world", "COMMENT"},
		{TokAction, "Show Message Box", "Display.ShowMessageBox"},
		{TokSubflowEnd, "", "EndRegion"},
	}

	if len(tokens) != len(expected) {
		t.Fatalf("expected %d tokens, got %d", len(expected), len(tokens))
	}

	for i, exp := range expected {
		if tokens[i].Kind != exp.kind {
			t.Errorf("token %d: expected kind %d, got %d", i, exp.kind, tokens[i].Kind)
		}
		if tokens[i].Name != exp.name {
			t.Errorf("token %d: expected name %q, got %q", i, exp.name, tokens[i].Name)
		}
		if tokens[i].RawType != exp.rawType {
			t.Errorf("token %d: expected rawType %q, got %q", i, exp.rawType, tokens[i].RawType)
		}
	}
}

// TestTokenize_SpacedRegionSyntax verifies the spaced region form
// (`# Region "..."` / `# EndRegion`) is classified as a subflow boundary, not a
// comment. reComment's "#\s+.*" alternative matches these lines, so without the
// region guard in classifyComment the subflow start/end is stolen and the block
// tree is corrupted with spurious unclosed-block errors.
func TestTokenize_SpacedRegionSyntax(t *testing.T) {
	input := `# Region "Main"
    Display.ShowMessageBox Message: 'hi'
# EndRegion`

	tokens := Tokenize(input)

	expected := []struct {
		kind    TokenKind
		rawType string
	}{
		{TokSubflowStart, "Region"},
		{TokAction, "Display.ShowMessageBox"},
		{TokSubflowEnd, "EndRegion"},
	}

	if len(tokens) != len(expected) {
		t.Fatalf("expected %d tokens, got %d (%+v)", len(expected), len(tokens), tokens)
	}
	for i, exp := range expected {
		if tokens[i].Kind != exp.kind {
			t.Errorf("token %d: expected kind %d, got %d (rawType %q)", i, exp.kind, tokens[i].Kind, tokens[i].RawType)
		}
		if tokens[i].RawType != exp.rawType {
			t.Errorf("token %d: expected rawType %q, got %q", i, exp.rawType, tokens[i].RawType)
		}
	}
}

func TestTokenize_Loops(t *testing.T) {
	input := `#Region "Main"
    LOOP FOREACH Item IN %List%
        Display.ShowMessageBox Message: %Item%
    END
#EndRegion`

	tokens := Tokenize(input)

	kinds := []TokenKind{TokSubflowStart, TokLoopStart, TokAction, TokEnd, TokSubflowEnd}
	for i, k := range kinds {
		if tokens[i].Kind != k {
			t.Errorf("token %d: expected kind %d, got %d (content: %q)", i, k, tokens[i].Kind, tokens[i].Content)
		}
	}
}

func TestTokenize_Conditions(t *testing.T) {
	input := `#Region "Main"
    IF %x% > 10
        Display.ShowMessageBox Message: $'''big'''
    ELSE
        Display.ShowMessageBox Message: $'''small'''
    END
#EndRegion`

	tokens := Tokenize(input)

	kinds := []TokenKind{TokSubflowStart, TokConditionStart, TokAction, TokConditionElse, TokAction, TokEnd, TokSubflowEnd}
	for i, k := range kinds {
		if i >= len(tokens) {
			t.Fatalf("token %d: out of range (have %d tokens)", i, len(tokens))
		}
		if tokens[i].Kind != k {
			t.Errorf("token %d: expected kind %d, got %d (content: %q)", i, k, tokens[i].Kind, tokens[i].Content)
		}
	}
}

func TestTokenize_SetVariable(t *testing.T) {
	input := `#Region "Main"
    SET MyVar TO $'''hello'''
#EndRegion`

	tokens := Tokenize(input)

	if tokens[1].Kind != TokAction {
		t.Errorf("expected TokAction, got %d", tokens[1].Kind)
	}
	if tokens[1].RawType != "SET" {
		t.Errorf("expected rawType SET, got %q", tokens[1].RawType)
	}
}

func TestTokenize_Wait(t *testing.T) {
	input := `#Region "Main"
    WAIT 5
#EndRegion`

	tokens := Tokenize(input)

	if tokens[1].Kind != TokAction {
		t.Errorf("expected TokAction, got %d", tokens[1].Kind)
	}
	if tokens[1].RawType != "WAIT" {
		t.Errorf("expected rawType WAIT, got %q", tokens[1].RawType)
	}
}

func TestTokenize_OnBlockError(t *testing.T) {
	input := `#Region "Main"
    ON BLOCK ERROR
        Display.ShowMessageBox Message: $'''error'''
    END
#EndRegion`

	tokens := Tokenize(input)

	kinds := []TokenKind{TokSubflowStart, TokErrorHandlerStart, TokAction, TokEnd, TokSubflowEnd}
	for i, k := range kinds {
		if tokens[i].Kind != k {
			t.Errorf("token %d: expected kind %d, got %d (content: %q)", i, k, tokens[i].Kind, tokens[i].Content)
		}
	}
}

func TestTokenize_EmptyLines(t *testing.T) {
	input := `#Region "Main"

    COMMENT  hello

#EndRegion`

	tokens := Tokenize(input)

	emptyCount := 0
	for _, tok := range tokens {
		if tok.Kind == TokEmpty {
			emptyCount++
		}
	}
	if emptyCount != 2 {
		t.Errorf("expected 2 empty tokens, got %d", emptyCount)
	}
}

func TestTokenize_Indent(t *testing.T) {
	input := `#Region "Main"
    Display.ShowMessageBox Message: $'''level1'''
        Display.ShowMessageBox Message: $'''level2'''
#EndRegion`

	tokens := Tokenize(input)

	if tokens[1].Indent != 4 {
		t.Errorf("token 1: expected indent 4, got %d", tokens[1].Indent)
	}
	if tokens[2].Indent != 8 {
		t.Errorf("token 2: expected indent 8, got %d", tokens[2].Indent)
	}
}

func TestTokenize_DottedActions(t *testing.T) {
	input := `#Region "Main"
    DateTime.GetCurrentDateTime DateTimeFormat: DateTime.DateTimeFormat.DateAndTime => CurrentDateTime
    Variables.SetVariable NewValue: %CurrentDateTime% Name: FormattedDate
    WebAutomation.LaunchBrowser BrowserType: WebAutomation.BrowserType.Chrome => Browser
#EndRegion`

	tokens := Tokenize(input)

	rawTypes := []string{"Region", "DateTime.GetCurrentDateTime", "Variables.SetVariable", "WebAutomation.LaunchBrowser", "EndRegion"}
	for i, rt := range rawTypes {
		if tokens[i].RawType != rt {
			t.Errorf("token %d: expected rawType %q, got %q", i, rt, tokens[i].RawType)
		}
	}
}

func TestTokenize_BOM(t *testing.T) {
	input := "\xEF\xBB\xBF#Region \"Main\"\n    COMMENT  test\n#EndRegion"
	tokens := Tokenize(input)

	if tokens[0].Kind != TokSubflowStart {
		t.Errorf("expected TokSubflowStart, got %d", tokens[0].Kind)
	}
	if tokens[0].Name != "Main" {
		t.Errorf("expected name Main, got %q", tokens[0].Name)
	}
}

func TestTokenize_Switch(t *testing.T) {
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

	tokens := Tokenize(input)

	kinds := []TokenKind{
		TokSubflowStart,
		TokSwitchStart,
		TokCase,
		TokAction,
		TokCase,
		TokAction,
		TokDefault,
		TokAction,
		TokEnd,
		TokSubflowEnd,
	}
	if len(tokens) != len(kinds) {
		t.Fatalf("expected %d tokens, got %d", len(kinds), len(tokens))
	}
	for i, k := range kinds {
		if tokens[i].Kind != k {
			t.Errorf("token %d: expected kind %d, got %d (content: %q)", i, k, tokens[i].Kind, tokens[i].Content)
		}
	}

	// SWITCH token name should carry the expression
	if tokens[1].RawType != "SWITCH" {
		t.Errorf("expected rawType SWITCH, got %q", tokens[1].RawType)
	}
	// CASE token
	if tokens[2].RawType != "CASE" {
		t.Errorf("expected rawType CASE, got %q", tokens[2].RawType)
	}
	// DEFAULT token
	if tokens[6].RawType != "DEFAULT" {
		t.Errorf("expected rawType DEFAULT, got %q", tokens[6].RawType)
	}
}

func TestTokenize_GotoLabel(t *testing.T) {
	input := `#Region "Main"
    GOTO 'SkipSection'
    Display.ShowMessageBox Message: $'''should be skipped'''
    LABEL 'SkipSection'
    Display.ShowMessageBox Message: $'''after skip'''
#EndRegion`

	tokens := Tokenize(input)

	// Find GOTO and LABEL tokens
	var gotoIdx, labelIdx = -1, -1
	for i, tok := range tokens {
		if tok.RawType == "GOTO" {
			gotoIdx = i
		}
		if tok.RawType == "LABEL" {
			labelIdx = i
		}
	}

	if gotoIdx == -1 {
		t.Fatal("expected a GOTO token")
	}
	if labelIdx == -1 {
		t.Fatal("expected a LABEL token")
	}
	if tokens[gotoIdx].Kind != TokAction {
		t.Errorf("GOTO: expected TokAction, got %d", tokens[gotoIdx].Kind)
	}
	if tokens[labelIdx].Kind != TokAction {
		t.Errorf("LABEL: expected TokAction, got %d", tokens[labelIdx].Kind)
	}
	// Name should be the label target (without quotes)
	if tokens[gotoIdx].Name != "SkipSection" {
		t.Errorf("GOTO Name = %q, want %q", tokens[gotoIdx].Name, "SkipSection")
	}
	if tokens[labelIdx].Name != "SkipSection" {
		t.Errorf("LABEL Name = %q, want %q", tokens[labelIdx].Name, "SkipSection")
	}
}

func TestTokenize_BlockComment_SingleLine(t *testing.T) {
	input := `#Region "Main"
    /# This is a single-line block comment #/
    Display.ShowMessageBox Message: $'''after comment'''
#EndRegion`

	tokens := Tokenize(input)

	// Expect: SubflowStart, Comment, Action, SubflowEnd
	kinds := []TokenKind{TokSubflowStart, TokComment, TokAction, TokSubflowEnd}
	if len(tokens) != len(kinds) {
		t.Fatalf("expected %d tokens, got %d", len(kinds), len(tokens))
	}
	for i, k := range kinds {
		if tokens[i].Kind != k {
			t.Errorf("token %d: expected kind %d, got %d (content: %q)", i, k, tokens[i].Kind, tokens[i].Content)
		}
	}
	if tokens[1].RawType != "COMMENT" {
		t.Errorf("block comment token: expected rawType COMMENT, got %q", tokens[1].RawType)
	}
}

func TestTokenize_BlockComment_MultiLine(t *testing.T) {
	input := `#Region "Main"
    /# This is a
    multi-line block comment #/
    Display.ShowMessageBox Message: $'''after comment'''
#EndRegion`

	tokens := Tokenize(input)

	// Expect: SubflowStart, Comment (multi-line merged), Action, SubflowEnd
	kinds := []TokenKind{TokSubflowStart, TokComment, TokAction, TokSubflowEnd}
	if len(tokens) != len(kinds) {
		t.Fatalf("expected %d tokens, got %d", len(kinds), len(tokens))
	}
	if tokens[1].Kind != TokComment {
		t.Errorf("expected multi-line block comment to be TokComment, got %d", tokens[1].Kind)
	}
	if tokens[1].RawType != "COMMENT" {
		t.Errorf("expected rawType COMMENT, got %q", tokens[1].RawType)
	}
}

func TestTokenize_SplitCamelCase(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"ShowMessageBox", "Show Message Box"},
		{"GetCurrentDateTime", "Get Current Date Time"},
		{"LaunchBrowser", "Launch Browser"},
		{"HTTPSRequest", "HTTPS Request"},
		{"HTTPServer", "HTTP Server"},
		{"OpenXMLFile", "Open XML File"},
		{"simpleword", "simpleword"},
		{"A", "A"},
	}
	for _, tc := range cases {
		got := splitCamelCase(tc.input)
		if got != tc.want {
			t.Errorf("splitCamelCase(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
