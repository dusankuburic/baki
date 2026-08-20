package analyzer

import (
	"strings"

	"pad-core/models"
)

type DuplicateLabelRule struct{}

func (r *DuplicateLabelRule) ID() string   { return "duplicate-label" }
func (r *DuplicateLabelRule) Name() string { return "Duplicate label" }
func (r *DuplicateLabelRule) Description() string {
	return "Two or more LABEL blocks share the same name, causing ambiguous GOTO resolution — only the first is used."
}
func (r *DuplicateLabelRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *DuplicateLabelRule) Category() string                 { return "Logic" }

func (r *DuplicateLabelRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.RawType != "LABEL" {
		return nil
	}
	labelName := block.Name
	if labelName == "" {
		return nil
	}

	// Count LABEL blocks sharing this name (case-insensitive, matching the
	// goto-antipattern resolver and buildLabelIndex exactly) from the
	// precomputed LabelNameCount index — previously this rescanned
	// ctx.AllBlocks per LABEL block (O(labels × blocks)).
	lower := strings.ToLower(labelName)
	count := ctx.LabelNameCount[lower]
	if count <= 1 {
		return nil
	}

	// Only emit on the first occurrence (by document order) to avoid N identical
	// findings. ctx.LabelByName is built in document order by buildLabelIndex
	// (keyed with strings.ToLower), so it deterministically points to the first.
	first := ctx.LabelByName[lower]
	if first != nil && block.ID != first.ID {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Duplicate label",
		Description: "Label '" + labelName + "' is defined " + itoa(count) + " times. GOTO resolves to the first definition — the others are unreachable targets.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Rename the duplicate labels so each has a unique name.",
		Metadata:    map[string]interface{}{"label": labelName, "count": count},
	}}
}

func init() { registerRule(&DuplicateLabelRule{}) }
