package archtest

import (
	"go/ast"
	"strings"
	"testing"
)

// TestArch_SystemReadsPinned: a system read is a storage call that reaches every tenant's rows by design — a
// FlowFilter with AllFlows, or GetFlowBaselinesBatch and RuleSetsByDigest, which the governance scanner runs on the
// pool with no RLS scope. Only the background jobs listed here may issue one; a request path reads through the scoped methods instead
// (ADR 0013). The storage packages themselves, which implement and contract-test them, are not counted.
func TestArch_SystemReadsPinned(t *testing.T) {
	got := map[string]int{}
	for _, f := range sourceFiles(t) {
		if strings.HasPrefix(f.path, "internal/storage/") {
			continue
		}
		if n := countSystemReads(f.ast); n > 0 {
			got[f.path] = n
		}
	}
	checkRatchet(t, "system reads", got, map[string]int{
		"internal/scanner/scanner.go": 2, // AllFlows: the sweep walks every flow; GetFlowBaselinesBatch per page
		"internal/scanner/notices.go": 1, // RuleSetsByDigest: per page, the recorded rule sets the sweep has not resolved
	})
}

// countSystemReads counts the references to GetFlowBaselinesBatch and RuleSetsByDigest, called or passed as a method
// value, and the places that set FlowFilter.AllFlows: a keyed composite-literal field or an assignment to a selector
// named AllFlows. Reading the field does not count.
func countSystemReads(f *ast.File) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "GetFlowBaselinesBatch" || x.Sel.Name == "RuleSetsByDigest" {
				n++
			}
		case *ast.KeyValueExpr:
			if k, ok := x.Key.(*ast.Ident); ok && k.Name == "AllFlows" {
				n++
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "AllFlows" {
					n++
				}
			}
		}
		return true
	})
	return n
}
