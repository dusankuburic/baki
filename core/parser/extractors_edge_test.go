package parser

import "testing"

// Edge cases for the %%-escape / string-literal interaction in variable
// extraction: literal percent signs inside quoted strings must not produce
// phantom variables.
func TestExtractVariables_EscapedPercentInStrings(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`'50%%'`, 0},
		{`$'''%%'''`, 0},
		{`"100%% done"`, 0},
		{`'%%' %Real%`, 1}, // escaped percent next to a real reference
	}
	for _, c := range cases {
		got := extractVariables(c.in)
		if len(got) != c.want {
			t.Errorf("extractVariables(%q) = %v, want %d variable(s)", c.in, got, c.want)
		}
	}
}

// Malformed property strings must not panic and must return something sane.
func TestParseProperties_MalformedInput(t *testing.T) {
	cases := []string{
		`Key: $'''unclosed`,
		`a:b:c:d:e`,
		`:`,
		`: value with no key`,
		`Key:`,
		`'''`,
	}
	for _, c := range cases {
		got := parseProperties(c) // must not panic
		if got == nil {
			t.Errorf("parseProperties(%q) returned nil map", c)
		}
	}
}
