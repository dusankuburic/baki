package parser

import (
	"testing"
)

func TestParseProperties_OutputVar(t *testing.T) {
	props := parseProperties("DateTime.GetCurrentDateTime DateTimeFormat: DateTime.DateTimeFormat.DateAndTime => CurrentDateTime")
	if props["_output"] != "CurrentDateTime" {
		t.Errorf("expected _output=CurrentDateTime, got %q", props["_output"])
	}
}

func TestParseProperties_KeyValuePairs(t *testing.T) {
	props := parseProperties("Display.ShowMessageBox Message: $'''Hello World''' Icon: Display.Icon.None Buttons: Display.Buttons.OK => ButtonPressed")
	if props["Message"] != "Hello World" {
		t.Errorf("expected Message='Hello World', got %q", props["Message"])
	}
	if props["_output"] != "ButtonPressed" {
		t.Errorf("expected _output=ButtonPressed, got %q", props["_output"])
	}
}

func TestParseProperties_SimpleKeyValue(t *testing.T) {
	props := parseProperties("Variables.SetVariable NewValue: %CurrentDate% Name: FormattedDate")
	if props["NewValue"] != "%CurrentDate%" {
		t.Errorf("expected NewValue='%%CurrentDate%%', got %q", props["NewValue"])
	}
}

func TestParseProperties_NoProperties(t *testing.T) {
	props := parseProperties("SomeAction")
	if len(props) != 0 {
		t.Errorf("expected 0 properties, got %d", len(props))
	}
}

func TestParseProperties_QuotedString(t *testing.T) {
	props := parseProperties(`WebAutomation.NavigateTo Url: $'''https://example.com''' BrowserInstance: %Browser%`)
	if props["Url"] != "https://example.com" {
		t.Errorf("expected Url='https://example.com', got %q", props["Url"])
	}
}

func TestExtractVariables(t *testing.T) {
	vars := extractVariables("SET MyVar TO %SomeValue% + %OtherValue%")
	if len(vars) != 2 {
		t.Fatalf("expected 2 variables, got %d", len(vars))
	}
	if vars[0] != "SomeValue" {
		t.Errorf("expected vars[0]=SomeValue, got %q", vars[0])
	}
	if vars[1] != "OtherValue" {
		t.Errorf("expected vars[1]=OtherValue, got %q", vars[1])
	}
}

func TestExtractVariables_Duplicates(t *testing.T) {
	vars := extractVariables("%x% + %x% + %y%")
	if len(vars) != 2 {
		t.Errorf("expected 2 unique variables, got %d: %v", len(vars), vars)
	}
}

func TestExtractVariables_None(t *testing.T) {
	vars := extractVariables("some plain text without vars")
	if len(vars) != 0 {
		t.Errorf("expected 0 variables, got %d", len(vars))
	}
}

func TestExtractVariables_Indexed(t *testing.T) {
	// Index access %ListVar[1]% should extract only the base variable name "ListVar".
	vars := extractVariables("%ListVar[1]%")
	if len(vars) != 1 || vars[0] != "ListVar" {
		t.Errorf("expected [ListVar], got %v", vars)
	}
}

func TestExtractVariables_PercentEscape(t *testing.T) {
	// %% is an escaped literal percent and must not be treated as a variable delimiter.
	vars := extractVariables("50%%")
	if len(vars) != 0 {
		t.Errorf("expected no variables, got %v", vars)
	}
}

func TestExtractVariables_RowIndexed(t *testing.T) {
	// %CurrentRow['Status']% should extract "CurrentRow", not "CurrentRow['Status']".
	vars := extractVariables("%CurrentRow['Status']%")
	if len(vars) != 1 || vars[0] != "CurrentRow" {
		t.Errorf("expected [CurrentRow], got %v", vars)
	}
}

func TestStripQuotes(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"$'''hello'''", "hello"},
		{"'''hello'''", "hello"},
		{`'hello'`, "hello"},
		{`"hello"`, "hello"},
		{"hello", "hello"},
		{"$''''''", ""},
	}
	for _, tt := range tests {
		got := stripQuotes(tt.input)
		if got != tt.expected {
			t.Errorf("stripQuotes(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
