package scrubber

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// parseScrubbed parses src and returns the document the AI sees after ScrubDocument.
func parseScrubbed(t *testing.T, src string) *models.FlowDocument {
	t.Helper()
	doc, err := parser.ParseText(src, "t", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := ScrubDocument(doc)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	return got
}

// findBlock returns the first block (depth-first) whose Name has prefix.
func findBlock(blocks []models.Block, prefix string) *models.Block {
	for i := range blocks {
		if strings.HasPrefix(blocks[i].Name, prefix) {
			return &blocks[i]
		}
		if b := findBlock(blocks[i].Children, prefix); b != nil {
			return b
		}
	}
	return nil
}

const nameOpaqueToken = "Zq8vN2pL5xR9tW3kY7mB4cF6hJ1dG0sA2eU5iO8p"

// nameFlowSrc holds a literal in every block kind whose Name is the raw source line: an IF and an ELSE IF
// comparing a credential variable, a CASE under a credential SWITCH, a LOOP WHILE, a WAIT expression and a
// comment, next to the same shapes on variables that hold no credential.
const nameFlowSrc = "#Region \"Main\"\n" +
	"    # Ops handover, vault reference " + nameOpaqueToken + " (ask Ops)\n" +
	"    IF %DbPassword% = $'''Pr0d!Pass''' THEN\n" +
	"        Display.ShowMessageDialog Title: $'''Hi''' Message: $'''ok'''\n" +
	"    ELSE IF %DbPassword% = 'Alt!Pass' THEN\n" +
	"        WAIT 2\n" +
	"    END\n" +
	"    IF %Status% = $'''Active''' THEN\n" +
	"        WAIT 1\n" +
	"    END\n" +
	"    SWITCH %Password%\n" +
	"        CASE = $'''CaseSecret1'''\n" +
	"            WAIT 1\n" +
	"        DEFAULT\n" +
	"            WAIT 1\n" +
	"    END\n" +
	"    SWITCH %Region%\n" +
	"        CASE = $'''EMEA'''\n" +
	"            WAIT 1\n" +
	"    END\n" +
	"    LOOP WHILE (%VaultToken%) <> ($'''LoopSecret1''')\n" +
	"        WAIT 1\n" +
	"    END\n" +
	"    LOOP WHILE (%Retries%) < (3)\n" +
	"        WAIT (%Retries%)\n" +
	"    END\n" +
	"    IF %Note% = $'''" + nameOpaqueToken + "''' THEN\n" +
	"        WAIT 1\n" +
	"    END\n" +
	"#EndRegion\n"

// TestScrubDocument_NameLiterals: a literal compared with a credential variable lives in Block.Name alone
// (the line has no `key: value` pair, and the `%` closing the variable keeps the regex pass from matching).
// ScrubDocument masks it, keeps the variable reference, and leaves the same shape on an ordinary variable.
func TestScrubDocument_NameLiterals(t *testing.T) {
	got := parseScrubbed(t, nameFlowSrc)
	blocks := got.Subflows[0].Blocks

	cases := []struct {
		prefix string
		secret string
		keep   []string
	}{
		{"IF %DbPassword%", "Pr0d!Pass", []string{"%DbPassword%", "IF %DbPassword% = $'''"}},
		{"ELSE IF %DbPassword%", "Alt!Pass", []string{"%DbPassword%", "'"}},
		{"CASE = $'''CaseSecret1", "CaseSecret1", []string{"CASE = $'''"}},
		{"LOOP WHILE (%VaultToken%)", "LoopSecret1", []string{"%VaultToken%", "<>"}},
		// A literal that passes the entropy check is masked on any variable.
		{"IF %Note%", nameOpaqueToken, []string{"%Note%"}},
	}
	for _, c := range cases {
		b := findBlock(blocks, c.prefix)
		if strings.HasPrefix(c.prefix, "CASE") {
			// The CASE's own prefix is what the mask rewrites; reach it through its SWITCH.
			if sw := findBlock(blocks, "SWITCH %Password%"); sw != nil && len(sw.Children) > 0 {
				b = &sw.Children[0]
			}
		}
		if b == nil {
			t.Fatalf("block %q not found", c.prefix)
		}
		if strings.Contains(b.Name, c.secret) {
			t.Errorf("%s: literal %q survives in Name %q", c.prefix, c.secret, b.Name)
		}
		if !strings.Contains(b.Name, redacted) {
			t.Errorf("%s: no mask in Name %q", c.prefix, b.Name)
		}
		for _, k := range c.keep {
			if !strings.Contains(b.Name, k) {
				t.Errorf("%s: %q lost from Name %q", c.prefix, k, b.Name)
			}
		}
	}

	for _, want := range []string{
		"IF %Status% = $'''Active'''",
		"CASE = $'''EMEA'''",
		"LOOP WHILE (%Retries%) < (3)",
		"WAIT (%Retries%)",
		"SWITCH %Password%",
	} {
		if b := findBlock(blocks, want[:len(want)-1]); b == nil || b.Name != want {
			t.Errorf("benign Name %q changed: %+v", want, b)
		}
	}
}

// TestScrubDocument_CommentTokens: a comment keeps its words and loses the opaque token.
func TestScrubDocument_CommentTokens(t *testing.T) {
	got := parseScrubbed(t, nameFlowSrc)
	b := findBlock(got.Subflows[0].Blocks, "Ops handover")
	if b == nil {
		t.Fatal("comment block not found")
	}
	if strings.Contains(b.Name, nameOpaqueToken) {
		t.Errorf("comment token survives: %q", b.Name)
	}
	if want := "Ops handover, vault reference [REDACTED] (ask Ops)"; b.Name != want {
		t.Errorf("comment Name = %q, want %q", b.Name, want)
	}
	// A multi-line block comment is one Name; the token inside is masked, the lines around it stay.
	multi := parseScrubbed(t, "#Region \"Main\"\n    /# Runbook\n    key "+nameOpaqueToken+"\n    end #/\n    WAIT 1\n#EndRegion\n")
	c := findBlock(multi.Subflows[0].Blocks, "/# Runbook")
	if c == nil {
		t.Fatal("block comment not found")
	}
	if strings.Contains(c.Name, nameOpaqueToken) || !strings.Contains(c.Name, "Runbook") || !strings.Contains(c.Name, "end #/") {
		t.Errorf("block comment = %q", c.Name)
	}
}

// TestScrubDocument_CommentKeyValue: the regex pass runs on a comment too, so `password=…` in one is masked.
func TestScrubDocument_CommentKeyValue(t *testing.T) {
	got := parseScrubbed(t, "#Region \"Main\"\n    # legacy: password=hunter2secret42 for the old box\n    WAIT 1\n#EndRegion\n")
	b := findBlock(got.Subflows[0].Blocks, "legacy")
	if b == nil {
		t.Fatal("comment not found")
	}
	if strings.Contains(b.Name, "hunter2secret42") || !strings.Contains(b.Name, "for the old box") {
		t.Errorf("comment = %q", b.Name)
	}
}

// TestScrubDocument_NamePassLeavesTheOriginal: the Name pass writes to the copy only.
func TestScrubDocument_NamePassLeavesTheOriginal(t *testing.T) {
	doc, err := parser.ParseText(nameFlowSrc, "t", int64(len(nameFlowSrc)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ScrubDocument(doc); err != nil {
		t.Fatal(err)
	}
	if b := findBlock(doc.Subflows[0].Blocks, "IF %DbPassword%"); b == nil || !strings.Contains(b.Name, "Pr0d!Pass") {
		t.Errorf("original IF Name changed: %+v", b)
	}
}

// TestScrubDocument_SetTargetCredentialLike: `SET DbPassword TO …` names a credential the way the
// analyzer's hardcoded-credential rule reads names (a substring), not only the exact sensitiveFieldNames.
func TestScrubDocument_SetTargetCredentialLike(t *testing.T) {
	got := parseScrubbed(t, "#Region \"Main\"\n    SET DbPassword TO $'''Pr0d!Pass'''\n    SET Status TO $'''Active'''\n#EndRegion\n")
	blocks := got.Subflows[0].Blocks
	if v := blocks[0].Properties["_value"]; v != redacted {
		t.Errorf("SET DbPassword _value = %q, want %s", v, redacted)
	}
	if v := blocks[1].Properties["_value"]; v != "$'''Active'''" {
		t.Errorf("SET Status _value = %q, want unchanged", v)
	}
}

// TestScrubSourceText_NameLiterals: the source-text scrub masks the same header literals in the raw text
// and keeps the headers that hold no secret.
func TestScrubSourceText_NameLiterals(t *testing.T) {
	got := ScrubSourceText(nameFlowSrc)
	for _, secret := range []string{"Pr0d!Pass", "Alt!Pass", "CaseSecret1", "LoopSecret1", nameOpaqueToken} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survives in the source text", secret)
		}
	}
	for _, keep := range []string{
		"IF %DbPassword% = $'''[REDACTED]''' THEN",
		"CASE = $'''[REDACTED]'''",
		"LOOP WHILE (%VaultToken%) <> ($'''[REDACTED]''')",
		"IF %Status% = $'''Active''' THEN",
		"CASE = $'''EMEA'''",
		"# Ops handover, vault reference [REDACTED] (ask Ops)",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q missing from:\n%s", keep, got)
		}
	}
}

// allNames joins every Name of a block tree, one per line, in document order.
func allNames(blocks []models.Block) string {
	var b strings.Builder
	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		for i := range blocks {
			b.WriteString(blocks[i].Name)
			b.WriteByte('\n')
			walk(blocks[i].Children)
		}
	}
	walk(blocks)
	return b.String()
}

// TestScrub_CredentialReferenceInPath: a credential read off a custom object or a datarow (%Creds.Password%,
// %Creds['Password']%) names it in the property path, which the parser's variable list drops (Variables
// holds Creds alone). The three scrubbers read the reference off the line and agree; the index key, a
// field name and not a value, survives, and so does a path that names no credential.
func TestScrub_CredentialReferenceInPath(t *testing.T) {
	src := "#Region \"Main\"\n" +
		"    IF %Creds.Password% = $'''DottedSecret1''' THEN\n" +
		"        WAIT 1\n" +
		"    END\n" +
		"    IF %Creds['Password']% = $'''IndexedSecret1''' THEN\n" +
		"        WAIT 1\n" +
		"    END\n" +
		"    SWITCH %Creds.Password%\n" +
		"        CASE = $'''DottedCase1'''\n" +
		"            WAIT 1\n" +
		"    END\n" +
		"    IF %Creds.Region% = $'''EMEA''' THEN\n" +
		"        WAIT 1\n" +
		"    END\n" +
		"    IF %Rows[0]['Password']% = $'''RowIndexed1''' THEN\n" +
		"        WAIT 1\n" +
		"    END\n" +
		"    SWITCH %Rows[%i%]['Password']%\n" +
		"        CASE = $'''RowCase1'''\n" +
		"            WAIT 1\n" +
		"    END\n" +
		"    SET Password TO %Creds['Password']%\n" +
		"#EndRegion\n"
	secrets := []string{"DottedSecret1", "IndexedSecret1", "DottedCase1", "RowIndexed1", "RowCase1"}
	keep := []string{
		"%Creds.Password% = $'''[REDACTED]'''",
		"%Creds['Password']% = $'''[REDACTED]'''",
		"CASE = $'''[REDACTED]'''",
		"%Creds.Region% = $'''EMEA'''",
		"%Rows[0]['Password']% = $'''[REDACTED]'''",
		"SWITCH %Rows[%i%]['Password']%",
	}
	check := func(path, got string, keep []string) {
		t.Helper()
		for _, s := range secrets {
			if strings.Contains(got, s) {
				t.Errorf("%s: %q survives in:\n%s", path, s, got)
			}
		}
		for _, k := range keep {
			if !strings.Contains(got, k) {
				t.Errorf("%s: %q missing from:\n%s", path, k, got)
			}
		}
	}
	doc := parseScrubbed(t, src)
	if b := findBlock(doc.Subflows[0].Blocks, "IF %Creds.Password%"); b == nil || len(b.Variables) != 1 || b.Variables[0] != "Creds" {
		t.Fatalf("the parser's variable list changed, the fixture no longer covers a dropped path: %+v", b)
	}
	check("ScrubDocument", allNames(doc.Subflows[0].Blocks), keep)
	check("ScrubSourceText", ScrubSourceText(src), append(keep, "SET Password TO %Creds['Password']%"))
	check("ScrubFreeText", ScrubFreeText(src), append(keep, "SET Password TO %Creds['Password']%"))
}

// TestIsCredentialLikeName pins the name set against the analyzer's: a substring match on the normalized
// name, and a superset of the exact property-key set.
func TestIsCredentialLikeName(t *testing.T) {
	for _, name := range []string{"DbPassword", "VaultToken", "client_secret", "Api-Key", "pwd2", "UserCredential", "AccessKeyId", "ConnectionString", "PrivateKey"} {
		if !isCredentialLikeName(name) {
			t.Errorf("%q should name a credential", name)
		}
	}
	for _, name := range []string{"Status", "Region", "Retries", "Note", "UserName", "Count"} {
		if isCredentialLikeName(name) {
			t.Errorf("%q should not name a credential", name)
		}
	}
	for k := range sensitiveFieldNames {
		if !isCredentialLikeName(k) {
			t.Errorf("sensitive field name %q is not credential-like", k)
		}
	}
}

// TestScrubDocument_CommentLiterals: a comment is a raw line too, so a literal next to a credential variable
// is masked the way a header's is, and a quoted word about a credential is not.
func TestScrubDocument_CommentLiterals(t *testing.T) {
	got := parseScrubbed(t, "#Region \"Main\"\n"+
		"    # rotate %DbPassword% from 'Winter#2024' next week\n"+
		"    # the 'password' field is required (see \"Login\")\n"+
		"    WAIT 1\n#EndRegion\n")
	blocks := got.Subflows[0].Blocks
	if b := findBlock(blocks, "rotate"); b == nil || b.Name != "rotate %DbPassword% from '[REDACTED]' next week" {
		t.Errorf("comment with a credential variable = %+v", b)
	}
	if b := findBlock(blocks, "the 'password'"); b == nil || b.Name != "the 'password' field is required (see \"Login\")" {
		t.Errorf("comment about a credential changed: %+v", b)
	}
}

// TestScrubDocument_UnknownLine: a line the parser could not classify keeps its text in Name; its literals
// follow the header rule (the variables it names) and its bare words the comment rule (entropy).
func TestScrubDocument_UnknownLine(t *testing.T) {
	doc := &models.FlowDocument{Subflows: []models.Subflow{{ID: "sf", Name: "Main", Blocks: []models.Block{
		{ID: "u1", Type: models.BlockTypeUnknown, RawType: "UNKNOWN", Name: "FOO %Password% == 'Winter#2024'", Variables: []string{"Password"}},
		{ID: "u2", Type: models.BlockTypeUnknown, RawType: "UNKNOWN", Name: "BAR " + nameOpaqueToken + " 'x' done"},
		{ID: "u3", Type: models.BlockTypeUnknown, RawType: "UNKNOWN", Name: "BAZ %Status% == 'Active'", Variables: []string{"Status"}},
	}}}}
	got, err := ScrubDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{
		"FOO %Password% == '[REDACTED]'",
		"BAR [REDACTED] 'x' done",
		"BAZ %Status% == 'Active'",
	} {
		if name := got.Subflows[0].Blocks[i].Name; name != want {
			t.Errorf("block %d Name = %q, want %q", i, name, want)
		}
	}
}
