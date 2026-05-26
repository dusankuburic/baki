package models

import "testing"

// ---- DefaultSettings -------------------------------------------------------

func TestDefaultSettings_Version(t *testing.T) {
	s := DefaultSettings()
	if s.Version != 1 {
		t.Errorf("Version = %d, want 1", s.Version)
	}
}

func TestDefaultSettings_Theme(t *testing.T) {
	s := DefaultSettings()
	if s.Appearance.Theme != ThemeDark {
		t.Errorf("Theme = %q, want %q", s.Appearance.Theme, ThemeDark)
	}
}

func TestDefaultSettings_Layout(t *testing.T) {
	s := DefaultSettings()
	if s.Layout.SidebarWidth == 0 {
		t.Error("SidebarWidth should be non-zero")
	}
	if s.Layout.InspectorWidth == 0 {
		t.Error("InspectorWidth should be non-zero")
	}
}

func TestDefaultSettings_AIActiveProvider(t *testing.T) {
	s := DefaultSettings()
	if s.AI.ActiveProvider == "" {
		t.Error("ActiveProvider should not be empty")
	}
}

func TestDefaultSettings_AIProviders_ContainClaude(t *testing.T) {
	s := DefaultSettings()
	claude, ok := s.AI.Providers["claude"]
	if !ok {
		t.Fatal("expected 'claude' provider in Providers map")
	}
	if !claude.Enabled {
		t.Error("claude provider should be enabled by default")
	}
	if claude.DefaultModel == "" {
		t.Error("claude provider should have a DefaultModel")
	}
}

func TestDefaultSettings_AnalysisRules_NonEmpty(t *testing.T) {
	s := DefaultSettings()
	if len(s.Analysis.Rules) == 0 {
		t.Error("expected default analysis rules to be non-empty")
	}
}

func TestDefaultSettings_AnalysisRules_HardcodedCredential(t *testing.T) {
	s := DefaultSettings()
	rule, ok := s.Analysis.Rules["hardcoded-credential"]
	if !ok {
		t.Fatal("expected 'hardcoded-credential' rule in defaults")
	}
	if !rule.Enabled {
		t.Error("hardcoded-credential rule should be enabled by default")
	}
	if rule.Severity != "error" {
		t.Errorf("hardcoded-credential severity = %q, want %q", rule.Severity, "error")
	}
}

func TestDefaultSettings_Parser_Defaults(t *testing.T) {
	s := DefaultSettings()
	if s.Parser.MaxFileSizeMB == 0 {
		t.Error("MaxFileSizeMB should be non-zero")
	}
}

// ---- ThemeMode constants ---------------------------------------------------

func TestThemeModeConstants(t *testing.T) {
	tests := []struct {
		name string
		val  ThemeMode
	}{
		{"dark", ThemeDark},
		{"light", ThemeLight},
		{"system", ThemeSystem},
	}
	for _, tc := range tests {
		if string(tc.val) != tc.name {
			t.Errorf("Theme%s = %q, want %q", tc.name, tc.val, tc.name)
		}
	}
}

// ---- BlockType constants ---------------------------------------------------

func TestBlockTypeConstants_NonEmpty(t *testing.T) {
	types := []BlockType{
		BlockTypeAction, BlockTypeLoop, BlockTypeCondition,
		BlockTypeSubflow, BlockTypeErrorHandler, BlockTypeComment,
		BlockTypeVariable, BlockTypeWait, BlockTypeElse,
		BlockTypeCase, BlockTypeDefault, BlockTypeBlock,
		BlockTypeSwitch, BlockTypeEnd, BlockTypeUnknown,
	}
	seen := make(map[BlockType]bool)
	for _, bt := range types {
		if bt == "" {
			t.Errorf("BlockType constant is empty string")
		}
		if seen[bt] {
			t.Errorf("duplicate BlockType value: %q", bt)
		}
		seen[bt] = true
	}
}

// ---- Severity constants ----------------------------------------------------

func TestSeverityConstants(t *testing.T) {
	if SeverityError != "error" {
		t.Errorf("SeverityError = %q, want %q", SeverityError, "error")
	}
	if SeverityWarning != "warning" {
		t.Errorf("SeverityWarning = %q, want %q", SeverityWarning, "warning")
	}
	if SeverityInfo != "info" {
		t.Errorf("SeverityInfo = %q, want %q", SeverityInfo, "info")
	}
}

// ---- ChangeType constants --------------------------------------------------

func TestChangeTypeConstants(t *testing.T) {
	if ChangeNone != "none" {
		t.Errorf("ChangeNone = %q", ChangeNone)
	}
	if ChangeAdded != "added" {
		t.Errorf("ChangeAdded = %q", ChangeAdded)
	}
	if ChangeRemoved != "removed" {
		t.Errorf("ChangeRemoved = %q", ChangeRemoved)
	}
	if ChangeModified != "modified" {
		t.Errorf("ChangeModified = %q", ChangeModified)
	}
}

// ---- Struct zero values / initialization -----------------------------------

func TestFlowDocument_ZeroValue(t *testing.T) {
	var doc FlowDocument
	if doc.ID != "" || doc.Name != "" {
		t.Error("zero FlowDocument should have empty ID and Name")
	}
}

func TestFinding_SeverityField(t *testing.T) {
	f := Finding{
		Severity: SeverityError,
		Title:    "test finding",
	}
	if f.Severity != SeverityError {
		t.Errorf("Severity = %q, want %q", f.Severity, SeverityError)
	}
}

func TestBlock_TypeField(t *testing.T) {
	b := Block{
		ID:   "b1",
		Type: BlockTypeAction,
	}
	if b.Type != BlockTypeAction {
		t.Errorf("Type = %q, want %q", b.Type, BlockTypeAction)
	}
}
