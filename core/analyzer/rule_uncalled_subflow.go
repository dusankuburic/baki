package analyzer

import (
	"pad-core/models"
)

type UncalledSubflowRule struct{}

func (r *UncalledSubflowRule) ID() string   { return "uncalled-subflow" }
func (r *UncalledSubflowRule) Name() string { return "Uncalled subflow" }
func (r *UncalledSubflowRule) Description() string {
	return "Subflow that is never called by any other subflow and is not the entry point — dead code at the flow level."
}
func (r *UncalledSubflowRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *UncalledSubflowRule) Category() string                 { return "Logic" }

func (r *UncalledSubflowRule) CheckFlow(ctx *RuleContext) []models.Finding {
	fanIn := ctx.SubflowFanIn()
	var out []models.Finding
	for i := range ctx.Flow.Subflows {
		sf := &ctx.Flow.Subflows[i]
		// The entry-point subflow (first in document order) is the default
		// caller target — never flag it even with fan-in 0.
		if fanIn[sf.ID] > 0 || isEntryPoint(sf, ctx) {
			continue
		}
		// One finding per subflow, on its first block.
		anchor := subflowAnchor(ctx, sf)
		if anchor == nil {
			continue
		}
		out = append(out, models.Finding{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Uncalled subflow",
			Description: "Subflow '" + sf.Name + "' is never called by any other subflow and is not the entry point. It is dead code at the flow level.",
			BlockID:     anchor.ID,
			SubflowID:   sf.ID,
			Suggestion:  "Remove this subflow if it is no longer needed, or add a call to it from the entry point.",
			Metadata:    map[string]interface{}{"subflow": sf.Name},
		})
	}
	return out
}

// isEntryPoint returns true for the first subflow in document order (typically
// "Main") — the default entry point that is never flagged as uncalled.
func isEntryPoint(sf *models.Subflow, ctx *RuleContext) bool {
	if len(ctx.Flow.Subflows) > 0 && ctx.Flow.Subflows[0].ID == sf.ID {
		return true
	}
	// Also treat "Main" / "main" as entry point by convention.
	return sf.Name == "Main" || sf.Name == "main"
}

// No AutoFix: remove-block would strip one action per iteration, and
// subflows may be invoked dynamically.
func init() {
	registerRule(&UncalledSubflowRule{}, RuleMeta{
		Confidence:   models.ConfidenceMedium,
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
