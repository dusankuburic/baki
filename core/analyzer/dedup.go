package analyzer

import (
	"strings"

	"pad-core/models"
)

type FindingGroup = models.FindingGroup

// subjectKeys are the metadata keys that identify WHAT a finding concerns (the
// variable/property/resource it points at), as opposed to descriptive keys
// (pattern, self-assignment, …). Used to distinguish two findings on the same
// block+title that are genuinely about different things (so dedup keeps both)
// and to find related findings that share a subject across blocks.
//
// This is the same key list findingContentKey (engine.go) uses to build a
// finding's content key — aliased to subjectMetaKeys, the single source of
// truth, rather than a second literal that could silently drift out of sync.
var subjectKeys = subjectMetaKeys

// findingSubjects returns the subject identifiers a finding is about. A single
// rule writes at most one of these, but collecting all keeps the helper robust
// to future rules that set several. Empty subjects → a finding about the block
// as a whole (no specific variable/property/resource).
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

// findingDiscriminator distinguishes findings that share a block and title but
// concern different subjects — e.g. two uninitialized variables in the same
// block. Without it, (BlockID, Title) dedup silently drops all but the first.
func findingDiscriminator(f models.Finding) string {
	return strings.Join(findingSubjects(f), "\x00")
}

func DeduplicateFindings(findings []models.Finding) ([]models.Finding, []FindingGroup) {
	type key struct {
		blockID string
		title   string
		subject string
	}
	seen := make(map[key]int)
	var deduped []models.Finding
	groups := make(map[string][]models.Finding)

	for _, f := range findings {
		k := key{blockID: f.BlockID, title: f.Title, subject: findingDiscriminator(f)}
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
			tk := f.Title + "\x00" + findingDiscriminator(f)
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

	if deduped == nil {
		deduped = []models.Finding{}
	}
	if resultGroups == nil {
		resultGroups = []FindingGroup{}
	}

	return deduped, resultGroups
}

// FindRelatedFindings returns findings on OTHER blocks that share a subject
// (variable/property/resource) with any finding on blockID — e.g. a
// resource-leak's variable used by an uninitialized-variable finding elsewhere.
// Previously this read Metadata["variables"] ([]string), which no rule writes;
// every rule writes the singular keys (variable/property/resource), so it
// returned empty for every real flow.
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
