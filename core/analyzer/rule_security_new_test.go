package analyzer

import (
	"testing"

	"pad-core/models"
)

func secBlock(id, rawType string, props map[string]string) *models.Block {
	return &models.Block{
		ID:         id,
		Name:       id,
		Type:       models.BlockTypeAction,
		RawType:    rawType,
		Properties: props,
	}
}

func TestHardcodedUICoordinates(t *testing.T) {
	r := &HardcodedUICoordinatesRule{}
	ctx := &RuleContext{}

	if got := r.Check(secBlock("b1", "UIAutomation.Click", map[string]string{"X": "320", "Y": "240"}), ctx); len(got) != 1 {
		t.Errorf("hardcoded literal coords: got %d findings, want 1", len(got))
	}
	// Variable coordinate is NOT flagged (it's parameterized).
	if got := r.Check(secBlock("b2", "UIAutomation.Click", map[string]string{"X": "%ClickX%", "Y": "%ClickY%"}), ctx); len(got) != 0 {
		t.Errorf("variable coords: got %d findings, want 0", len(got))
	}
	// Non-UI action with X/Y is not flagged.
	if got := r.Check(secBlock("b3", "Variables.Set", map[string]string{"X": "100"}), ctx); len(got) != 0 {
		t.Errorf("non-UI action: got %d findings, want 0", len(got))
	}
}

func TestCommandInjectionRisk(t *testing.T) {
	r := &CommandInjectionRiskRule{}
	ctx := &RuleContext{}

	if got := r.Check(secBlock("b1", "System.RunDOSCommand", map[string]string{"CommandLine": "ping %HostName%"}), ctx); len(got) != 1 {
		t.Errorf("command with var arg: got %d findings, want 1", len(got))
	}
	if got := r.Check(secBlock("b2", "System.RunDOSCommand", map[string]string{"CommandLine": "dir C:\\logs"}), ctx); len(got) != 0 {
		t.Errorf("static command: got %d findings, want 0", len(got))
	}
	if got := r.Check(secBlock("b3", "Variables.Set", map[string]string{"Value": "%x%"}), ctx); len(got) != 0 {
		t.Errorf("non-command action: got %d findings, want 0", len(got))
	}
	// %% is PAD's literal-percent escape, not a variable reference — it must not
	// trigger a false-positive command-injection finding (the fixer can't
	// resolve it, so the finding would be unactionable).
	if got := r.Check(secBlock("b4", "System.RunDOSCommand", map[string]string{"CommandLine": "echo 100%% complete"}), ctx); len(got) != 0 {
		t.Errorf("command with literal %%: got %d findings, want 0", len(got))
	}
	if got := r.Check(secBlock("b1", "System.RunDOSCommand", map[string]string{"CommandLine": "ping %HostName%"}), ctx)[0].Severity; got != models.SeverityError {
		t.Errorf("severity = %v, want error (command injection is high-severity)", got)
	}
}

func TestInsecureHttpUrl(t *testing.T) {
	r := &InsecureHttpUrlRule{}
	ctx := &RuleContext{}

	if got := r.Check(secBlock("b1", "HTTPClient.InvokeService", map[string]string{"Url": "http://example.com/api"}), ctx); len(got) != 1 {
		t.Errorf("http:// URL: got %d findings, want 1", len(got))
	}
	if got := r.Check(secBlock("b2", "HTTPClient.InvokeService", map[string]string{"Url": "https://example.com/api"}), ctx); len(got) != 0 {
		t.Errorf("https:// URL: got %d findings, want 0", len(got))
	}
	// A variable URL can't be statically classified as cleartext — must not flag.
	if got := r.Check(secBlock("b3", "HTTPClient.InvokeService", map[string]string{"Url": "%Endpoint%"}), ctx); len(got) != 0 {
		t.Errorf("variable URL: got %d findings, want 0", len(got))
	}
	if got := r.Check(secBlock("b4", "Variables.Set", map[string]string{"Url": "http://x"}), ctx); len(got) != 0 {
		t.Errorf("non-network action: got %d findings, want 0", len(got))
	}
}

func TestPathTraversalRisk(t *testing.T) {
	r := &PathTraversalRiskRule{}
	ctx := &RuleContext{}

	if got := r.Check(secBlock("b1", "File.OpenTextFile", map[string]string{"Path": "%UserPath%"}), ctx); len(got) != 1 {
		t.Errorf("variable path: got %d findings, want 1", len(got))
	}
	// Static literal path is NOT flagged here (left to the portability rule).
	if got := r.Check(secBlock("b2", "File.OpenTextFile", map[string]string{"Path": "C:\\data\\file.txt"}), ctx); len(got) != 0 {
		t.Errorf("static path: got %d findings, want 0", len(got))
	}
	if got := r.Check(secBlock("b3", "Variables.Set", map[string]string{"Path": "%x%"}), ctx); len(got) != 0 {
		t.Errorf("non-file action: got %d findings, want 0", len(got))
	}
}

// TestPathTraversalRisk_RealPADPropertyNames: the rule keyed on property names
// PAD never emits (destination, folderpath, filepath…); the parser emits File,
// Files, FilesToCopy, FolderPath, Folder, SpecialFolder. It never fired on a
// real flow.
func TestPathTraversalRisk_RealPADPropertyNames(t *testing.T) {
	r := &PathTraversalRiskRule{}
	ctx := &RuleContext{}
	cases := []struct {
		name  string
		typ   string
		props map[string]string
		want  int
	}{
		{"File.WriteText File", "File.WriteText", map[string]string{"File": "%OutPath%", "TextToWrite": "x"}, 1},
		{"File.Delete Files", "File.Delete", map[string]string{"Files": "%Target%"}, 1},
		{"File.Copy Destination", "File.Copy", map[string]string{"FilesToCopy": "C:\\a.txt", "Destination": "%Dest%"}, 1},
		{"Folder.Create FolderPath", "Folder.Create", map[string]string{"FolderPath": "%Root%", "FolderName": "x"}, 1},
		{"variable only in a non-path property", "File.WriteText", map[string]string{"File": "C:\\log.txt", "TextToWrite": "%Body%"}, 0},
		{"enum special folder", "Folder.GetSpecialFolder", map[string]string{"SpecialFolder": "Folder.SpecialFolder.Desktop"}, 0},
		{"literal percent escape", "File.WriteText", map[string]string{"File": "C:\\100%% done.txt"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.Check(secBlock("b", c.typ, c.props), ctx); len(got) != c.want {
				t.Errorf("got %d findings, want %d", len(got), c.want)
			}
		})
	}
}

// TestCommandInjectionRisk_ScriptingActions: Scripting.* hands a string to an
// interpreter exactly as RunDOSCommand does, and was matched by no rule.
func TestCommandInjectionRisk_ScriptingActions(t *testing.T) {
	r := &CommandInjectionRiskRule{}
	ctx := &RuleContext{}
	for _, typ := range []string{"Scripting.RunPowershellScript", "Scripting.RunVBScript", "Scripting.RunPythonScript", "Scripting.RunJavascript"} {
		if got := r.Check(secBlock("b", typ, map[string]string{"Script": "Get-Item %Path%"}), ctx); len(got) != 1 {
			t.Errorf("%s with a variable: got %d findings, want 1", typ, len(got))
		}
		if got := r.Check(secBlock("b", typ, map[string]string{"Script": "Get-Date"}), ctx); len(got) != 0 {
			t.Errorf("%s without a variable: got %d findings, want 0", typ, len(got))
		}
	}
}
