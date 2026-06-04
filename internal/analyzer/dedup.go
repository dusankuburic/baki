package analyzer

import (
	"pad-analyzer/internal/models"
)

type FindingGroup = models.FindingGroup

func DeduplicateFindings(findings []models.Finding) ([]models.Finding, []FindingGroup) {
	type key struct {
		blockID string
		title   string
	}
	seen := make(map[key]int)
	var deduped []models.Finding
	groups := make(map[string][]models.Finding)

	for _, f := range findings {
		k := key{blockID: f.BlockID, title: f.Title}
		groups[f.BlockID] = append(groups[f.BlockID], f)

		if _, exists := seen[k]; exists {
			continue
		}
		seen[k] = len(deduped)
		deduped = append(deduped, f)
	}

	var resultGroups []FindingGroup
	for blockID, blockFindings := range groups {
		primary := blockFindings[0]
		dups := 0
		titleSet := make(map[string]bool)
		for _, f := range blockFindings {
			if titleSet[f.Title] {
				dups++
			}
			titleSet[f.Title] = true
		}
		resultGroups = append(resultGroups, FindingGroup{
			BlockID:        blockID,
			Findings:       blockFindings,
			Primary:        &primary,
			DuplicateCount: dups,
		})
	}

	if deduped == nil {
		deduped = []models.Finding{}
	}
	if resultGroups == nil {
		resultGroups = []FindingGroup{}
	}

	return deduped, resultGroups
}

func FindRelatedFindings(findings []models.Finding, blockID string) []models.Finding {
	var related []models.Finding
	blockVars := make(map[string]bool)

	for _, f := range findings {
		if f.BlockID == blockID && f.Metadata != nil {
			if vars, ok := f.Metadata["variables"].([]string); ok {
				for _, v := range vars {
					blockVars[v] = true
				}
			}
		}
	}

	for _, f := range findings {
		if f.BlockID == blockID {
			continue
		}
		if f.Metadata != nil {
			if vars, ok := f.Metadata["variables"].([]string); ok {
				for _, v := range vars {
					if blockVars[v] {
						related = append(related, f)
						break
					}
				}
			}
		}
	}

	if related == nil {
		related = []models.Finding{}
	}
	return related
}
