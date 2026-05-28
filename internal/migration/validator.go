package migration

import (
	"encoding/json"
	"fmt"
	"strings"

	"pad-analyzer/internal/storage/interfaces"
)

// Validator checks flow documents for issues before migration.
type Validator struct{}

// NewValidator returns a Validator.
func NewValidator() *Validator { return &Validator{} }

// ValidateFlow returns a slice of human-readable problem descriptions.
// An empty slice means the flow is valid.
func (v *Validator) ValidateFlow(flow *interfaces.FlowDocument) []string {
	var errs []string

	if strings.TrimSpace(flow.ID) == "" {
		errs = append(errs, "flow ID is empty")
	}
	if strings.TrimSpace(flow.Name) == "" {
		errs = append(errs, fmt.Sprintf("flow %q has no name", flow.ID))
	}
	if len(flow.Content) > 0 && !json.Valid(flow.Content) {
		errs = append(errs, fmt.Sprintf("flow %q content is not valid JSON", flow.ID))
	}
	if flow.Metadata.BlockCount < 0 {
		errs = append(errs, fmt.Sprintf("flow %q has negative block count", flow.ID))
	}

	return errs
}

// RepairFlow attempts lightweight, in-place fixes for common problems.
// It returns the (possibly modified) flow and a slice of applied fixes.
func (v *Validator) RepairFlow(flow *interfaces.FlowDocument) (*interfaces.FlowDocument, []string) {
	var fixes []string

	if strings.TrimSpace(flow.Name) == "" {
		flow.Name = fmt.Sprintf("Untitled (%s)", flow.ID)
		fixes = append(fixes, "assigned default name")
	}

	if len(flow.Content) == 0 {
		flow.Content = []byte("{}")
		fixes = append(fixes, "set empty content to {}")
	} else if !json.Valid(flow.Content) {
		// Trim trailing commas and whitespace — covers the most common corruption
		repaired := strings.TrimSpace(string(flow.Content))
		repaired = strings.TrimRight(repaired, ",")
		if json.Valid([]byte(repaired)) {
			flow.Content = []byte(repaired)
			fixes = append(fixes, "repaired trailing comma in JSON content")
		}
	}

	return flow, fixes
}
