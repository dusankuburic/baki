package analyzer

import (
	"pad-analyzer/internal/models"
)

type FlowComparison = models.FlowComparison
type SubflowComparison = models.SubflowComparison
type BlockComparison = models.BlockComparison

func CompareFlows(docA, docB *models.FlowDocument) *FlowComparison {
	result := &FlowComparison{
		FlowAID: docA.ID,
		FlowBID: docB.ID,
	}

	sfMapA := make(map[string]*models.Subflow)
	for i := range docA.Subflows {
		sfMapA[docA.Subflows[i].Name] = &docA.Subflows[i]
	}
	sfMapB := make(map[string]*models.Subflow)
	for i := range docB.Subflows {
		sfMapB[docB.Subflows[i].Name] = &docB.Subflows[i]
	}

	seen := make(map[string]bool)
	var allNames []string
	for name := range sfMapA {
		allNames = append(allNames, name)
		seen[name] = true
	}
	for name := range sfMapB {
		if !seen[name] {
			allNames = append(allNames, name)
			seen[name] = true
		}
	}

	var totalShared, totalAdded, totalRemoved int

	for _, name := range allNames {
		sfA, hasA := sfMapA[name]
		sfB, hasB := sfMapB[name]
		seen[name] = true

		comp := SubflowComparison{}
		if hasA && hasB {
			comp.SubflowA = sfA.ID
			comp.SubflowB = sfB.ID
			shared, added, removed := compareSubflowBlocks(sfA, sfB, &comp)
			totalShared += shared
			totalAdded += added
			totalRemoved += removed
			total := shared + added + removed
			if total > 0 {
				comp.Similarity = float64(shared) / float64(total)
			}
		} else if hasA {
			comp.SubflowA = sfA.ID
			for i := range sfA.Blocks {
				comp.BlockDiffs = append(comp.BlockDiffs, BlockComparison{
					BlockA: &sfA.Blocks[i],
					Change: "removed",
				})
				totalRemoved++
			}
		} else {
			comp.SubflowB = sfB.ID
			for i := range sfB.Blocks {
				comp.BlockDiffs = append(comp.BlockDiffs, BlockComparison{
					BlockB: &sfB.Blocks[i],
					Change: "added",
				})
				totalAdded++
			}
		}

		result.SubflowDiff = append(result.SubflowDiff, comp)
	}

	result.SharedBlocks = totalShared
	result.AddedBlocks = totalAdded
	result.RemovedBlocks = totalRemoved
	total := totalShared + totalAdded + totalRemoved
	if total > 0 {
		result.Similarity = float64(totalShared) / float64(total)
	}

	if result.SubflowDiff == nil {
		result.SubflowDiff = []SubflowComparison{}
	}

	return result
}

func compareSubflowBlocks(sfA, sfB *models.Subflow, comp *SubflowComparison) (shared, added, removed int) {
	blocksA := indexBlocksByName(sfA)
	blocksB := indexBlocksByName(sfB)

	for name, bA := range blocksA {
		if bB, ok := blocksB[name]; ok {
			sim := blockSimilarity(bA, bB)
			comp.BlockDiffs = append(comp.BlockDiffs, BlockComparison{
				BlockA:     bA,
				BlockB:     bB,
				Change:     "modified",
				Similarity: sim,
			})
			shared++
		} else {
			comp.BlockDiffs = append(comp.BlockDiffs, BlockComparison{
				BlockA: bA,
				Change: "removed",
			})
			removed++
		}
	}

	for name, bB := range blocksB {
		if _, ok := blocksA[name]; !ok {
			comp.BlockDiffs = append(comp.BlockDiffs, BlockComparison{
				BlockB: bB,
				Change: "added",
			})
			added++
		}
	}

	return shared, added, removed
}

func indexBlocksByName(sf *models.Subflow) map[string]*models.Block {
	m := make(map[string]*models.Block)
	walkSubflowBlocks(sf, func(b *models.Block) {
		if b.Name != "" {
			m[b.Name] = b
		}
	})
	return m
}

func blockSimilarity(a, b *models.Block) float64 {
	if a.RawType != b.RawType {
		return 0.0
	}
	if a.Type != b.Type {
		return 0.3
	}

	if len(a.Properties) == 0 && len(b.Properties) == 0 {
		return 1.0
	}

	matches := 0
	total := 0
	for k, vA := range a.Properties {
		total++
		if vB, ok := b.Properties[k]; ok && vA == vB {
			matches++
		}
	}
	for k := range b.Properties {
		if _, ok := a.Properties[k]; !ok {
			total++
		}
	}

	if total == 0 {
		return 1.0
	}
	return float64(matches) / float64(total)
}
