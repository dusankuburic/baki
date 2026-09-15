package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
)

// TestReplaceWithVariable_OnlyReplacesTheTargetedProperty is the regression gate
// for an unanchored rewrite.
//
// The patch carried the bare parsed value as Old — for a UI coordinate that is
// the string "50" — and applyReplace ran strings.ReplaceAll over the entire
// line. So fixing the X coordinate of
//
//	UIAutomation.PressButton Window: 'Order 1050' X: 50 Y: 150 Timeout: 30
//
// also rewrote the window title ("Order 10%input_x%") and the Y coordinate
// ("1%input_x%"). The result re-parses as valid Key: Value pairs and the finding
// is resolved, so the round-trip gate stayed green while the flow was corrupted.
func TestReplaceWithVariable_OnlyReplacesTheTargetedProperty(t *testing.T) {
	const line = "UIAutomation.PressButton Window: 'Order 1050' X: 50 Y: 150 Timeout: 30"
	block := &models.Block{
		LineNumber: 1,
		RawType:    "UIAutomation.PressButton",
		Properties: map[string]string{
			"Window":  "Order 1050",
			"X":       "50",
			"Y":       "150",
			"Timeout": "30",
		},
	}

	got := ApplyPatch(line, ReplaceWithVariablePatch(block, "X"))

	if !strings.Contains(got, "X: %input_x%") {
		t.Errorf("the targeted property was not replaced:\n%s", got)
	}
	if !strings.Contains(got, "Window: 'Order 1050'") {
		t.Errorf("the window title was rewritten by a replace aimed at X:\n%s", got)
	}
	if !strings.Contains(got, "Y: 150") {
		t.Errorf("the Y coordinate was rewritten by a replace aimed at X:\n%s", got)
	}
	if strings.Count(got, "%input_x%") != 1 {
		t.Errorf("expected exactly one substitution, got %d:\n%s", strings.Count(got, "%input_x%"), got)
	}
}

// TestReplaceWithVariable_DoesNotMatchAKeySuffix guards the anchor itself: "X"
// must not match inside "MaxX", or the anchoring just moves the corruption.
func TestReplaceWithVariable_DoesNotMatchAKeySuffix(t *testing.T) {
	const line = "UIAutomation.Drag MaxX: 900 X: 50"
	block := &models.Block{
		LineNumber: 1,
		Properties: map[string]string{"MaxX": "900", "X": "50"},
	}

	got := ApplyPatch(line, ReplaceWithVariablePatch(block, "X"))

	if !strings.Contains(got, "MaxX: 900") {
		t.Errorf("a replace aimed at X matched inside MaxX:\n%s", got)
	}
	if !strings.Contains(got, "X: %input_x%") {
		t.Errorf("the real X was not replaced:\n%s", got)
	}
}

// TestReplaceWithVariable_CredentialLiteralStillWorks keeps the fixer's original
// purpose intact — the hardcoded-credential case, where the value is a quoted
// literal rather than a bare number.
func TestReplaceWithVariable_CredentialLiteralStillWorks(t *testing.T) {
	const line = "Email.SendEmailSMTP Server: 'smtp.corp.local' Password: 'Wint3r2024' To: 'ops@corp'"
	block := &models.Block{
		LineNumber: 1,
		Properties: map[string]string{
			"Server":   "smtp.corp.local",
			"Password": "Wint3r2024",
			"To":       "ops@corp",
		},
	}

	got := ApplyPatch(line, ReplaceWithVariablePatch(block, "Password"))

	if !strings.Contains(got, "%input_password%") {
		t.Errorf("the credential was not replaced:\n%s", got)
	}
	if strings.Contains(got, "Wint3r2024") {
		t.Errorf("the credential literal is still present:\n%s", got)
	}
	if !strings.Contains(got, "Server: 'smtp.corp.local'") || !strings.Contains(got, "To: 'ops@corp'") {
		t.Errorf("unrelated properties were disturbed:\n%s", got)
	}
}

// TestReplaceWithVariable_RepeatedValueAcrossProperties is the sharpest case: the
// same literal appears under two keys, so an unanchored replace cannot tell them
// apart at all.
func TestReplaceWithVariable_RepeatedValueAcrossProperties(t *testing.T) {
	const line = "UIAutomation.PressButton X: 100 Y: 100"
	block := &models.Block{
		LineNumber: 1,
		Properties: map[string]string{"X": "100", "Y": "100"},
	}

	got := ApplyPatch(line, ReplaceWithVariablePatch(block, "Y"))

	if !strings.Contains(got, "X: 100") {
		t.Errorf("X was rewritten by a replace aimed at Y:\n%s", got)
	}
	if !strings.Contains(got, "Y: %input_y%") {
		t.Errorf("Y was not replaced:\n%s", got)
	}
}
