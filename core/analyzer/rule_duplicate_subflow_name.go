package analyzer

import (
	"pad-core/models"
)

type DuplicateSubflowNameRule struct{}

func (r *DuplicateSubflowNameRule) ID() string   { return "duplicate-subflow-name" }
func (r *DuplicateSubflowNameRule) Name() string { return "Duplicate subflow name" }
func (r *DuplicateSubflowNameRule) Description() string {
	return "Two or more subflows share the same name, causing ambiguous CALL resolution — only the first is used."
}
func (r *DuplicateSubflowNameRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *DuplicateSubflowNameRule) Category() string                 { return "Logic" }

func (r *DuplicateSubflowNameRule) CheckFlow(ctx *RuleContext) []models.Finding {
	dups := ctx.DuplicateSubflowNames()
	if len(dups) == 0 {
		return nil
	}
	var out []models.Finding
	for i := range ctx.Flow.Subflows {
		sf := &ctx.Flow.Subflows[i]
		if !dups[sf.Name] {
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
			Title:       "Duplicate subflow name",
			Description: "Subflow '" + sf.Name + "' shares its name with at least one other subflow. Only the first is used when CALL resolves by name — the others are silently unreachable.",
			BlockID:     anchor.ID,
			SubflowID:   sf.ID,
			Suggestion:  "Rename one of the duplicate subflows so each has a unique name.",
			Metadata:    map[string]interface{}{"subflow": sf.Name},
		})
	}
	return out
}

func init() {
	registerRule(&DuplicateSubflowNameRule{}, RuleMeta{
		Confidence:   models.ConfidenceMedium,
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
