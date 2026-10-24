package scrubber

import (
	"strings"
	"testing"
	"time"
)

const freeOpaqueToken = "Mx4bVq7nRt2kZw9yLp3dJc8sHf5gBa1eQu6iOy0r"

// freeTextSecrets are pasted PAD lines whose literal the regex pass alone lets through, each with the
// value that must be gone and the text that must survive around it.
var freeTextSecrets = []struct {
	name, in, secret string
	keep             []string
}{
	{
		name:   "SET of a credential",
		in:     "Why does the login step fail? The flow sets it like this:\nSET Password TO $'''Winter#2024-u7Qx'''",
		secret: "Winter#2024-u7Qx",
		keep:   []string{"Why does the login step fail?", "SET Password TO $'''[REDACTED]'''"},
	},
	{
		name:   "SET of a credential-like variable",
		in:     "    SET DbPassword TO 'Spring#2023'\n",
		secret: "Spring#2023",
		keep:   []string{"SET DbPassword TO '[REDACTED]'"},
	},
	{
		name:   "sensitive action field",
		in:     "WebAutomation.PopulateTextField BrowserInstance: %Browser% Control: appmask['Login']['Pwd'] Text: $'''Winter#2024'''",
		secret: "Winter#2024",
		keep:   []string{"appmask['Login']['Pwd']", "Text: $'''[REDACTED]'''"},
	},
	{
		name:   "sensitive action field is the typed value, not the locator",
		in:     "WebAutomation.FillTextByText Text: $'''login-field''' FillWith: $'''hunter2-secret'''",
		secret: "hunter2-secret",
		keep:   []string{"Text: $'''login-field'''", "FillWith: $'''[REDACTED]'''"},
	},
	{
		name:   "credential key with a PAD literal",
		in:     "Email.ConnectToSMTP Server: $'''smtp.example.com''' Password: $'''Winter#2024''' Port: 587",
		secret: "Winter#2024",
		keep:   []string{"Server: $'''smtp.example.com'''", "Password: $'''[REDACTED]'''", "Port: 587"},
	},
	{
		name:   "condition on a credential variable",
		in:     "The check is\nIF %DbPassword% = $'''Pr0d!Pass''' THEN\n  WAIT 1\nEND",
		secret: "Pr0d!Pass",
		keep:   []string{"IF %DbPassword% = $'''[REDACTED]''' THEN", "WAIT 1"},
	},
	{
		name:   "loop header on a credential variable",
		in:     "LOOP WHILE (%VaultToken%) <> ($'''LoopSecret1''')",
		secret: "LoopSecret1",
		keep:   []string{"LOOP WHILE (%VaultToken%) <> ($'''[REDACTED]''')"},
	},
	{
		name:   "opaque literal on any line",
		in:     "SET ServiceKey TO $'''" + freeOpaqueToken + "'''",
		secret: freeOpaqueToken,
		keep:   []string{"SET ServiceKey TO $'''[REDACTED]'''"},
	},
	{
		name:   "multi-line literal",
		in:     "SET Password TO $'''first-line\nsecond-line'''\nnext",
		secret: "first-line",
		keep:   []string{"SET Password TO $'''[REDACTED]'''\nnext"},
	},
	{
		name:   "literal after an unterminated triple quote",
		in:     "I can't get the ''' thing to work:\nSET Password TO 'Autumn#2025-k2Rw'",
		secret: "Autumn#2025-k2Rw",
		keep:   []string{"I can't get the ''' thing to work:", "SET Password TO '[REDACTED]'"},
	},
	{
		name:   "regex pass still runs",
		in:     "connection: Server=db;Uid=sa;Pwd=hunter2secret;",
		secret: "hunter2secret",
		keep:   []string{"Server=db;Uid=sa;Pwd=[REDACTED];"},
	},
	// One PAD line pasted into prose arrives as inline code, a list item, a quote or a numbered line.
	{
		name:   "inline code",
		in:     "the line is `SET Password TO 'Winter#2024-bt'` and it fails",
		secret: "Winter#2024-bt",
		keep:   []string{"the line is `SET Password TO '[REDACTED]'` and it fails"},
	},
	{
		name:   "list bullet",
		in:     "- SET Password TO 'Winter#2024-bt'\n* SET DbPassword TO 'Spring#2023-bt'",
		secret: "Winter#2024-bt",
		keep:   []string{"- SET Password TO '[REDACTED]'", "* SET DbPassword TO '[REDACTED]'"},
	},
	{
		name:   "block quote",
		in:     "> IF %DbPassword% = $'''Pr0d!Pass''' THEN",
		secret: "Pr0d!Pass",
		keep:   []string{"> IF %DbPassword% = $'''[REDACTED]''' THEN"},
	},
	{
		name:   "numbered line",
		in:     "12. LOOP WHILE (%VaultToken%) <> ('LoopSecret1')\n13) WAIT 1",
		secret: "LoopSecret1",
		keep:   []string{"12. LOOP WHILE (%VaultToken%) <> ('[REDACTED]')", "13) WAIT 1"},
	},
	{
		name:   "line-number prefix",
		in:     "Line 12: SET Password TO 'Winter#2024-bt'\nline 13: WAIT 1",
		secret: "Winter#2024-bt",
		keep:   []string{"Line 12: SET Password TO '[REDACTED]'", "line 13: WAIT 1"},
	},
	{
		name:   "bullet with inline code",
		in:     "- `WebAutomation.PopulateTextField Control: appmask['Login']['Pwd'] Text: $'''Winter#2024'''`",
		secret: "Winter#2024",
		keep:   []string{"appmask['Login']['Pwd']", "Text: $'''[REDACTED]'''`"},
	},
	{
		name:   "credential read off an object",
		in:     "IF %Creds.Password% = $'''DottedSecret1''' THEN\nIF %Creds['Password']% = 'IndexedSecret1' THEN",
		secret: "DottedSecret1",
		keep:   []string{"IF %Creds.Password% = $'''[REDACTED]''' THEN", "IF %Creds['Password']% = '[REDACTED]' THEN"},
	},
	{
		name:   "credential read off a datatable row",
		in:     "IF %Rows[0]['Password']% = $'''RowIndexed1''' THEN\nSWITCH %Rows[%i%]['Password']%\n    CASE = 'RowCase1'",
		secret: "RowIndexed1",
		keep:   []string{"IF %Rows[0]['Password']% = $'''[REDACTED]''' THEN", "SWITCH %Rows[%i%]['Password']%", "CASE = '[REDACTED]'"},
	},
	// The regex pass changes these literals before they are judged; the second pass must judge the same text.
	{
		name:   "connection string in a SET literal",
		in:     "SET ConnStr TO $'''Server=db;User=sa;Password=hunter2;'''",
		secret: "hunter2",
		keep:   []string{"SET ConnStr TO $'''"},
	},
	{
		name:   "key=value chain in a plain literal",
		in:     "SET Foo TO 'username=sa;password=aaaaaaaaaaaa;x'",
		secret: "aaaaaaaaaaaa",
		keep:   []string{"SET Foo TO '"},
	},
	// A pasted PAD comment gives up its opaque words, as it does in the flow.
	{
		name:   "comment with an opaque token",
		in:     "    # Ops handover, vault reference " + freeOpaqueToken + " (ask Ops)",
		secret: freeOpaqueToken,
		keep:   []string{"# Ops handover, vault reference [REDACTED] (ask Ops)"},
	},
}

// freeTextProse is text about credentials that carries none; every line must pass through unchanged.
var freeTextProse = []string{
	"the password field",
	"set Password to the vault value",
	"The flow reads %Password% from a secured input variable.",
	"SET NewPassword TO %Password%",
	"SET Username TO $'''admin'''",
	"IF %Status% = $'''Active''' THEN",
	"Don't store the password in the flow's text; isn't that what the vault is for?",
	"Display.ShowMessageDialog Title: $'''Login failed''' Message: $'''Check the password field'''",
	"password: \"%Password%\"",
	"Password: '${DB_PASSWORD}'",
	"SET Password TO $'''%Vault%'''",
	"WebAutomation.NavigateTo Url: $'''https://example.com/login?user=%Username%'''",
	"Use the variable password: %Password% in the action",
	"CASE = $'''EMEA'''",
	"IF %Creds.Region% = $'''EMEA''' THEN",
	"- SET Region TO 'EMEA'",
	"the line is `SET Status TO 'Active'` and it works",
	// Quoted names in prose clear the entropy bar as easily as a token does; the model needs them.
	"the action 'WebAutomation.PopulateTextField' fails after 'Variables.ConvertTextToNumber'",
	"open 'Invoice_2024-10-23_final.xlsx' from \"OrderProcessing_Main_v2.1\"",
	"the variable 'ThisIsAVeryLongVariableNameForTesting' is empty",
	"the key '{A1B2C3D4-E5F6-4A7B-8C9D-0E1F2A3B4C5D}' under 'HKEY_LOCAL_MACHINE/SOFTWARE/Vendor'",
	"the query 'SELECT*FROM[dbo].[Orders]WHERE' is cut off",
	"Note: '" + freeOpaqueToken + "'",
	"# Order processing flow — notes",
	"## What the 'Invoice' step does",
}

// TestScrubFreeText_LinearInTheText pins the cost shape: every rule reads a bounded tail of its line and the
// per-line facts are computed once, so a long single-line paste (minified JSON, a log line) costs milliseconds,
// not the seconds a per-literal rescan of the line prefix cost. Four times the text may cost about four times the
// time; the ratio, not the clock, is asserted, so a loaded machine or the race detector cannot fail it.
func TestScrubFreeText_LinearInTheText(t *testing.T) {
	timeOf := func(in string) time.Duration {
		start := time.Now()
		ScrubFreeText(in)
		return time.Since(start)
	}
	for _, shape := range []string{"'a' ", "IF %A% = 'x' ", "%V% 'x' ", "CASE 'x' ", `{"key":"value","id":"12345"},`} {
		small := timeOf(strings.Repeat(shape, 16<<10/len(shape)))
		large := timeOf(strings.Repeat(shape, 64<<10/len(shape)))
		t.Logf("%q: 16 KiB %v, 64 KiB %v", shape, small, large)
		if large > 8*small+50*time.Millisecond {
			t.Errorf("%q: 64 KiB on one line took %v against %v for 16 KiB — not linear", shape, large, small)
		}
	}
}

// TestScrubFreeText_EntropyNeedsPADShape: the entropy check reads a triple-quoted literal anywhere, since
// that form is PAD's and not prose's, and a '…' or "…" literal only on a line a PAD rule recognises: a
// SET, a condition or loop header, or an action's field. A quoted name in prose is kept (freeTextProse).
func TestScrubFreeText_EntropyNeedsPADShape(t *testing.T) {
	for _, in := range []string{
		"the value is $'''" + freeOpaqueToken + "''' in the designer",
		"SET ServiceKey TO '" + freeOpaqueToken + "'",
		"IF %Note% = '" + freeOpaqueToken + "' THEN",
		"Custom.Vault Note: '" + freeOpaqueToken + "'",
		"- `Custom.Vault Note: \"" + freeOpaqueToken + "\"`",
	} {
		if got := ScrubFreeText(in); strings.Contains(got, freeOpaqueToken) {
			t.Errorf("opaque literal survives:\n  in:  %q\n  out: %q", in, got)
		}
	}
}

func TestScrubFreeText_PastedPADLines(t *testing.T) {
	for _, c := range freeTextSecrets {
		t.Run(c.name, func(t *testing.T) {
			got := ScrubFreeText(c.in)
			if strings.Contains(got, c.secret) {
				t.Errorf("secret %q survives:\n  in:  %q\n  out: %q", c.secret, c.in, got)
			}
			for _, k := range c.keep {
				if !strings.Contains(got, k) {
					t.Errorf("%q lost:\n  in:  %q\n  out: %q", k, c.in, got)
				}
			}
		})
	}
}

func TestScrubFreeText_ProseIsKept(t *testing.T) {
	for _, in := range freeTextProse {
		if got := ScrubFreeText(in); got != in {
			t.Errorf("prose was masked:\n  in:  %q\n  out: %q", in, got)
		}
	}
}

// TestScrubFreeText_Idempotent: stored conversations and the scrub memo pass text through more than once,
// so a second pass must return the first pass's output byte for byte — over the samples, over prose, and
// over every three-fragment splice of them, where a mask of one fragment sits next to another's syntax.
func TestScrubFreeText_Idempotent(t *testing.T) {
	var fragments []string
	for _, c := range freeTextSecrets {
		fragments = append(fragments, c.in)
	}
	fragments = append(fragments, freeTextProse...)
	for _, tt := range secretSamples {
		fragments = append(fragments, tt.in)
	}
	check := func(x string) {
		t.Helper()
		once := ScrubFreeText(x)
		if twice := ScrubFreeText(once); twice != once {
			t.Errorf("not idempotent:\n  in:    %q\n  once:  %q\n  twice: %q", x, once, twice)
		}
	}
	for _, f := range fragments {
		check(f)
	}
	for i, a := range fragments {
		for j, b := range fragments {
			if i == j {
				continue
			}
			check(a + " " + b)
			check(a + "\n" + b + "\n" + fragments[(i+j)%len(fragments)])
		}
	}
}

// TestScrubFreeText_IsScrubTextWithoutLiterals: text with no quote in it has no PAD literal, and takes the
// regex pass alone, so the two functions agree on it.
func TestScrubFreeText_IsScrubTextWithoutLiterals(t *testing.T) {
	for _, tt := range secretSamples {
		if strings.ContainsAny(tt.in, `'"`) {
			continue
		}
		if got, want := ScrubFreeText(tt.in), ScrubText(tt.in); got != want {
			t.Errorf("ScrubFreeText(%q) = %q, ScrubText gives %q", tt.in, got, want)
		}
	}
	if got := ScrubFreeText(""); got != "" {
		t.Errorf("empty text became %q", got)
	}
}
