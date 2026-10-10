package analyzer

import (
	"pad-core/models"
)

type CircularSubflowRule struct{}

func (r *CircularSubflowRule) ID() string   { return "circular-subflow-dependency" }
func (r *CircularSubflowRule) Name() string { return "Circular subflow dependency" }
func (r *CircularSubflowRule) Description() string {
	return "Subflows that participate in a circular call chain, causing infinite recursion at runtime."
}
func (r *CircularSubflowRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *CircularSubflowRule) Category() string                 { return "Logic" }

func (r *CircularSubflowRule) CheckFlow(ctx *RuleContext) []models.Finding {
	circular := ctx.CircularSubflows()
	if len(circular) == 0 {
		return nil
	}
	var out []models.Finding
	for i := range ctx.Flow.Subflows {
		sf := &ctx.Flow.Subflows[i]
		if !circular[sf.ID] {
			continue
		}
		// One finding per cyclic subflow, on its first block (the subflow
		// header / first action).
		anchor := subflowAnchor(ctx, sf)
		if anchor == nil {
			continue
		}
		out = append(out, models.Finding{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Circular subflow dependency",
			Description: "Subflow '" + sf.Name + "' is part of a circular call chain. This will cause infinite recursion at runtime.",
			BlockID:     anchor.ID,
			SubflowID:   sf.ID,
			Suggestion:  "Break the cycle by removing one call in the chain or by introducing a termination condition.",
			Metadata:    map[string]interface{}{"subflow": sf.Name},
		})
	}
	return out
}

func init() {
	registerRule(&CircularSubflowRule{}, RuleMeta{
		Confidence:   models.ConfidenceHigh,
		IntroducedIn: 1,
		Revision:     2,
		Stage:        StageStable,
	})
}
