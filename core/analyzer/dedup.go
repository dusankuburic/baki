package analyzer

import (
	"slices"
	"strings"

	"pad-core/models"
)

type FindingGroup = models.FindingGroup

// subjectKeys are the metadata keys identifying WHAT a finding concerns
// (variable/property/resource) vs descriptive keys, distinguishing same
// block+title findings about different things; aliased to subjectMetaKeys
// (engine.go), the single source of truth.
var subjectKeys = subjectMetaKeys

// findingSubjects returns the subject identifiers a finding is about; empty
// means a finding about the block as a whole.
func findingSubjects(f models.Finding) []string {
	if f.Metadata == nil {
		return nil
	}
	var subjects []string
	for _, k := range subjectKeys {
		if v, ok := f.Metadata[k].(string); ok && v != "" {
			subjects = append(subjects, k+"="+v)
		}
	}
	return subjects
}

// findingDiscriminator distinguishes findings sharing a block and title but
// concerning different subjects — without it (BlockID, Title) dedup drops all
// but the first.
func findingDiscriminator(f models.Finding) string {
	return strings.Join(findingSubjects(f), "\x00")
}

func DeduplicateFindings(findings []models.Finding) ([]models.Finding, []FindingGroup) {
	return keptFindings(findings, dedupKeep(findings)), buildFindingGroups(findings)
}

// dedupKeep reports, per finding, whether DeduplicateFindings keeps it: first
// per (block, title, subject) key, later ones folded in. Split out so the
// engine can stamp IDs/fingerprints before filtering.
func dedupKeep(findings []models.Finding) []bool {
	type key struct {
		blockID string
		title   string
		subject string
	}
	seen := make(map[key]struct{}, len(findings))
	keep := make([]bool, len(findings))
	// Index-based: Finding is ~200 B, so the value copy on `for _, f := range`
	// doubled the pass's memory traffic on finding-dense reports.
	for i := range findings {
		f := &findings[i]
		k := key{blockID: f.BlockID, title: f.Title, subject: findingDiscriminator(*f)}
		if _, exists := seen[k]; exists {
			continue
		}
		seen[k] = struct{}{}
		keep[i] = true
	}
	return keep
}

// keptFindings returns the findings dedupKeep marked, in order (never nil).
func keptFindings(findings []models.Finding, keep []bool) []models.Finding {
	deduped := make([]models.Finding, 0, len(findings))
	for i := range findings {
		if keep[i] {
			deduped = append(deduped, findings[i])
		}
	}
	return deduped
}

// buildFindingGroups clusters ALL findings (duplicates included) per block;
// Primary is the block's first finding, DuplicateCount counts same-title,
// same-subject findings folded into an earlier one.
func buildFindingGroups(findings []models.Finding) []FindingGroup {
	groups := make(map[string][]models.Finding)
	for i := range findings {
		f := &findings[i]
		groups[f.BlockID] = append(groups[f.BlockID], *f)
	}

	var resultGroups []FindingGroup
	for blockID, blockFindings := range groups {
		primary := blockFindings[0]
		dups := 0
		titleSet := make(map[string]bool)
		for i := range blockFindings {
			f := &blockFindings[i]
			tk := f.Title + "\x00" + findingDiscriminator(*f)
			if titleSet[tk] {
				dups++
			}
			titleSet[tk] = true
		}
		resultGroups = append(resultGroups, FindingGroup{
			BlockID:        blockID,
			Findings:       blockFindings,
			Primary:        &primary,
			DuplicateCount: dups,
		})
	}

	if resultGroups == nil {
		resultGroups = []FindingGroup{}
	}

	// Map iteration is non-deterministic; sort by BlockID (then primary title)
	// for stable dedup-endpoint output and reproducible tests.
	slices.SortFunc(resultGroups, func(a, b FindingGroup) int {
		if c := strings.Compare(a.BlockID, b.BlockID); c != 0 {
			return c
		}
		at, bt := "", ""
		if a.Primary != nil {
			at = a.Primary.Title
		}
		if b.Primary != nil {
			bt = b.Primary.Title
		}
		return strings.Compare(at, bt)
	})

	return resultGroups
}

// FindRelatedFindings returns findings on OTHER blocks that share a subject
// (variable/property/resource) with any finding on blockID. (Previously read
// Metadata["variables"], which no rule writes, so it returned empty for every
// real flow; the singular keys are the real ones.)
func FindRelatedFindings(findings []models.Finding, blockID string) []models.Finding {
	blockSubjects := make(map[string]bool)
	for _, f := range findings {
		if f.BlockID == blockID {
			for _, s := range findingSubjects(f) {
				blockSubjects[s] = true
			}
		}
	}

	var related []models.Finding
	for _, f := range findings {
		if f.BlockID == blockID {
			continue
		}
		for _, s := range findingSubjects(f) {
			if blockSubjects[s] {
				related = append(related, f)
				break
			}
		}
	}

	if related == nil {
		related = []models.Finding{}
	}
	return related
}
