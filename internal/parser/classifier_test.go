package parser

import (
	"testing"

	"pad-analyzer/internal/models"
)

func TestClassifyBlockType(t *testing.T) {
	tests := []struct {
		rawType  string
		expected models.BlockType
	}{
		{"LOOP", models.BlockTypeLoop},
		{"Loop.ForEach", models.BlockTypeLoop},
		{"Loop.While", models.BlockTypeLoop},
		{"IF", models.BlockTypeCondition},
		{"Text.If", models.BlockTypeCondition},
		{"COMMENT", models.BlockTypeComment},
		{"SET", models.BlockTypeVariable},
		{"Variables.SetVariable", models.BlockTypeVariable},
		{"Variables.IncreaseVariable", models.BlockTypeVariable},
		{"WAIT", models.BlockTypeWait},
		{"Display.ShowMessageBox", models.BlockTypeAction},
		{"DateTime.GetCurrentDateTime", models.BlockTypeAction},
		{"WebAutomation.Click.Click", models.BlockTypeAction},
		{"OnBlockError", models.BlockTypeErrorHandler},
		{"SomeRandomAction", models.BlockTypeAction},
		{"", models.BlockTypeAction},
	}

	for _, tt := range tests {
		got := ClassifyBlockType(tt.rawType)
		if got != tt.expected {
			t.Errorf("ClassifyBlockType(%q) = %q, want %q", tt.rawType, got, tt.expected)
		}
	}
}
