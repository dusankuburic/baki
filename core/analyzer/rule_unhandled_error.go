package analyzer

import (
	"strings"

	"pad-core/models"
)

var falliblePrefixes = []string{
	"WebAutomation.",
	"UIAutomation.",
	"Excel.",
	"File.",
	"Http.",
	"Database.",
	"Email.",
	"Ftp.",
	"ActiveDirectory.",
	"SharePoint.",
	"OneDrive.",
	"Outlook.",
}

type UnhandledErrorRule struct{}

func (r *UnhandledErrorRule) ID() string   { return "unhandled-error" }
func (r *UnhandledErrorRule) Name() string { return "Unhandled error in fallible action" }
func (r *UnhandledErrorRule) Description() string {
	return "Actions that commonly fail (network, file, UI automation) where no error handler exists in the surrounding scope."
}
func (r *UnhandledErrorRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *UnhandledErrorRule) Category() string                 { return "Reliability" }

func (r *UnhandledErrorRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeAction {
		return nil
	}

	if !isFallible(block.RawType) {
		return nil
	}

	if HasErrorHandlerAncestor(ctx, block) {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Unhandled error in fallible action",
		Description: "This action commonly fails but has no error handler in its surrounding scope.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Wrap this action in a Try/Catch block or add error handler logic to recover from failures.",
	}}
}

func isFallible(rawType string) bool {
	for _, prefix := range falliblePrefixes {
		if strings.HasPrefix(rawType, prefix) {
			return true
		}
	}
	return false
}
