package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// suppressionFlow carries three directives: one current (rule still fires),
// one stale (rule no longer fires — target is fine now), one dangling (no
// following block).
const suppressionFlow = `#Region "Main"
    # pad-ignore[missing-timeout]
    WebAutomation.LaunchBrowser BrowserType: WebAutomation.BrowserType.Chrome
    COMMENT  fine now
    # pad-ignore[hardcoded-credential]
    SET Token TO 42
    # pad-ignore[unused-variable]
#EndRegion
`

// TestSuppressionsCmd pins R0/R1-5's inventory: directives listed with line
// numbers and targets; STALE ones (rule no longer fires / no target) flagged;
// --fail-on-stale gates; JSON round-trips.
func TestSuppressionsCmd(t *testing.T) {
	flow := writeTempFlow(t, suppressionFlow)

	out, stderr, code := runBakicli(t, "suppressions", flow)
	if code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "missing-timeout") || !strings.Contains(out, "Launch Browser") {
		t.Errorf("current directive missing from inventory:\n%s", out)
	}
	if !strings.Contains(out, "STALE") {
		t.Errorf("stale directives not flagged:\n%s", out)
	}
	if !strings.Contains(out, "no following block") && !strings.Contains(out, "directive(s)") {
		t.Errorf("inventory summary missing:\n%s", out)
	}

	// Stale present → gate fails.
	_, _, code = runBakicli(t, "suppressions", "-fail-on-stale", flow)
	if code != 1 {
		t.Errorf("fail-on-stale with stale directives: exit %d, want 1", code)
	}

	// JSON shape.
	out, _, _ = runBakicli(t, "suppressions", "-format", "json", flow)
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("want 3 entries, got %d: %s", len(entries), out)
	}
	stale := 0
	for _, e := range entries {
		if e["stale"] == true {
			stale++
		}
	}
	if stale != 2 { // hardcoded-credential no longer fires + dangling
		t.Errorf("stale count = %d, want 2: %s", stale, out)
	}
}

// TestSuppressionsCmd_CleanFlow: no directives → friendly output, gate passes.
func TestSuppressionsCmd_CleanFlow(t *testing.T) {
	flow := writeTempFlow(t, "#Region \"Main\"\n    COMMENT  nothing here\n#EndRegion\n")
	out, _, code := runBakicli(t, "suppressions", flow)
	if code != 0 || !strings.Contains(out, "no pad-ignore directives") {
		t.Errorf("clean flow: exit %d out=%q", code, out)
	}
	if _, _, code := runBakicli(t, "suppressions", "-fail-on-stale", flow); code != 0 {
		t.Errorf("clean flow fail-on-stale: exit %d, want 0", code)
	}
}

// TestSuppressionsCmd_Folder: folder targets work through loadFolder.
func TestSuppressionsCmd_Folder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Main.txt"), []byte(suppressionFlow), 0644); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := runBakicli(t, "suppressions", dir)
	if code != 0 {
		t.Fatalf("folder: exit %d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "missing-timeout") {
		t.Errorf("folder inventory missing directives:\n%s", out)
	}
}
