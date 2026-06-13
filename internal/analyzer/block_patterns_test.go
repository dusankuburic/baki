package analyzer

import (
	"testing"

	"pad-analyzer/internal/models"
)

func TestMatchesAny_Name(t *testing.T) {
	b := &models.Block{Name: "ExitSubflow"}
	if !matchesAny(b, terminatorNames) {
		t.Error("expected ExitSubflow to match terminator names")
	}
}

func TestMatchesAny_RawType(t *testing.T) {
	b := &models.Block{Name: "step1", RawType: "TerminateFlow"}
	if !matchesAny(b, terminatorNames) {
		t.Error("expected TerminateFlow raw type to match")
	}
}

func TestMatchesAny_NoMatch(t *testing.T) {
	b := &models.Block{Name: "HTTP Request", RawType: "HTTP"}
	if matchesAny(b, terminatorNames) {
		t.Error("expected no match for HTTP block")
	}
}

func TestIsExitLoop(t *testing.T) {
	tests := []struct {
		name    string
		rawType string
		expect  bool
	}{
		{"ExitLoop", "", true},
		{"Exit loop", "", true},
		{"Break", "", true},
		{"Return", "", true},
		{"End flow", "", true},
		{"ExitSubflow", "", true},
		{"Exit subflow", "", true},
		{"HTTP Request", "HTTP", false},
		{"Continue", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &models.Block{Name: tt.name, RawType: tt.rawType}
			if got := isExitLoop(b); got != tt.expect {
				t.Errorf("isExitLoop(name=%q, rawType=%q) = %v, want %v", tt.name, tt.rawType, got, tt.expect)
			}
		})
	}
}

func TestOutputVar_PrimaryOutput(t *testing.T) {
	b := &models.Block{Properties: map[string]string{"_output": "result", "_var": "fallback"}}
	if v := outputVar(b); v != "result" {
		t.Errorf("expected 'result', got %q", v)
	}
}

func TestOutputVar_FallbackVar(t *testing.T) {
	b := &models.Block{Properties: map[string]string{"_var": "fallback"}}
	if v := outputVar(b); v != "fallback" {
		t.Errorf("expected 'fallback', got %q", v)
	}
}

func TestOutputVar_NilProperties(t *testing.T) {
	b := &models.Block{}
	if v := outputVar(b); v != "" {
		t.Errorf("expected empty string for nil properties, got %q", v)
	}
}

func TestOutputVar_EmptyProperties(t *testing.T) {
	b := &models.Block{Properties: map[string]string{}}
	if v := outputVar(b); v != "" {
		t.Errorf("expected empty string, got %q", v)
	}
}
