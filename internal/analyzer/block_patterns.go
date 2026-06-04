package analyzer

import (
	"strings"

	"pad-analyzer/internal/models"
)

// terminatorNames are the block names / raw types that indicate a flow
// termination point (exit subflow, end flow, return, etc.).
// These are vendor-specific strings emitted by Power Automate when exporting
// flows — they cannot be a typed enum because the parser maps them to
// BlockTypeAction with these raw names rather than a distinct BlockType.
var terminatorNames = []string{
	"ExitSubflow",
	"Exit subflow",
	"End flow",
	"EndFlow",
	"Return",
	"TerminateFlow",
}

// exitLoopNames are the block names / raw types that indicate an early loop exit.
// Centralized here to avoid duplication between engine.go and rule_infinite_loop.go.
var exitLoopNames = []string{
	"ExitLoop",
	"Exit loop",
	"Break",
	"Return",
	"End flow",
	"ExitSubflow",
	"Exit subflow",
}

// matchesAny returns true when b.Name or b.RawType contains any pattern string.
func matchesAny(b *models.Block, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(b.Name, p) || strings.Contains(b.RawType, p) {
			return true
		}
	}
	return false
}

// isExitLoop reports whether b would cause early exit from a loop.
func isExitLoop(b *models.Block) bool {
	return matchesAny(b, exitLoopNames)
}

// outputVar returns the variable a block writes: the _output property, or the
// _var property as a fallback. Returns "" when the block writes no variable.
// Centralizes the _output→_var fallback that was duplicated across rules.
func outputVar(b *models.Block) string {
	if b.Properties == nil {
		return ""
	}
	if v := b.Properties["_output"]; v != "" {
		return v
	}
	return b.Properties["_var"]
}
