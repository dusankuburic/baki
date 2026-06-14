package parser

import (
	"pad-core/models"
)

func DiffFlows(oldDoc, newDoc *models.FlowDocument) *models.FlowDiff {
	diff := &models.FlowDiff{
		OldID: oldDoc.ID,
		NewID: newDoc.ID,
	}

	oldSubflows := make(map[string]*models.Subflow)
	for i := range oldDoc.Subflows {
		oldSubflows[oldDoc.Subflows[i].Name] = &oldDoc.Subflows[i]
	}

	newSubflows := make(map[string]*models.Subflow)
	for i := range newDoc.Subflows {
		newSubflows[newDoc.Subflows[i].Name] = &newDoc.Subflows[i]
	}

	// Process new and common subflows
	for _, newSf := range newDoc.Subflows {
		if oldSf, ok := oldSubflows[newSf.Name]; ok {
			// Common subflow - diff blocks
			diff.Subflows = append(diff.Subflows, diffSubflow(oldSf, &newSf))
		} else {
			// Added subflow
			diff.Subflows = append(diff.Subflows, models.SubflowDiff{
				Name:   newSf.Name,
				Change: models.ChangeAdded,
				Blocks: wrapBlocksAsDiff(newSf.Blocks, models.ChangeAdded),
			})
		}
	}

	// Process removed subflows
	for _, oldSf := range oldDoc.Subflows {
		if _, ok := newSubflows[oldSf.Name]; !ok {
			diff.Subflows = append(diff.Subflows, models.SubflowDiff{
				Name:   oldSf.Name,
				Change: models.ChangeRemoved,
				Blocks: wrapBlocksAsDiff(oldSf.Blocks, models.ChangeRemoved),
			})
		}
	}

	return diff
}

func wrapBlocksAsDiff(blocks []models.Block, change models.ChangeType) []models.BlockDiff {
	res := make([]models.BlockDiff, 0)
	for i := range blocks {
		bd := models.BlockDiff{Change: change}
		if change == models.ChangeAdded {
			bd.New = &blocks[i]
		} else {
			bd.Old = &blocks[i]
		}
		res = append(res, bd)
		
		// Recursively wrap children? 
		// Actually, for a completely added/removed subflow, 
		// we might just want a flat list or the root blocks.
		// Let's stick to root blocks for now, but usually diffs are flat.
		if len(blocks[i].Children) > 0 {
			res = append(res, wrapBlocksAsDiff(blocks[i].Children, change)...)
		}
	}
	return res
}

func diffSubflow(oldSf, newSf *models.Subflow) models.SubflowDiff {
	oldBlocks := flattenBlocks(oldSf.Blocks)
	newBlocks := flattenBlocks(newSf.Blocks)

	matrix := lcs(oldBlocks, newBlocks)
	blocks := backtrack(matrix, oldBlocks, newBlocks, len(oldBlocks), len(newBlocks))

	change := models.ChangeNone
	for _, b := range blocks {
		if b.Change != models.ChangeNone {
			change = models.ChangeModified
			break
		}
	}

	return models.SubflowDiff{
		Name:   newSf.Name,
		Change: change,
		Blocks: blocks,
	}
}

func flattenBlocks(blocks []models.Block) []models.Block {
	res := make([]models.Block, 0)
	for i := range blocks {
		res = append(res, blocks[i])
		if len(blocks[i].Children) > 0 {
			res = append(res, flattenBlocks(blocks[i].Children)...)
		}
	}
	return res
}

// Simple LCS for block comparison
func lcs(a, b []models.Block) [][]int {
	m := len(a)
	n := len(b)
	matrix := make([][]int, m+1)
	for i := 0; i <= m; i++ {
		matrix[i] = make([]int, n+1)
	}

	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if blocksEqual(&a[i-1], &b[j-1]) {
				matrix[i][j] = matrix[i-1][j-1] + 1
			} else {
				matrix[i][j] = max(matrix[i-1][j], matrix[i][j-1])
			}
		}
	}
	return matrix
}

func backtrack(matrix [][]int, a, b []models.Block, i, j int) []models.BlockDiff {
	if i > 0 && j > 0 && blocksEqual(&a[i-1], &b[j-1]) {
		res := backtrack(matrix, a, b, i-1, j-1)
		return append(res, models.BlockDiff{
			Change: models.ChangeNone,
			Old:    &a[i-1],
			New:    &b[j-1],
		})
	}
	if j > 0 && (i == 0 || matrix[i][j-1] >= matrix[i-1][j]) {
		res := backtrack(matrix, a, b, i, j-1)
		return append(res, models.BlockDiff{
			Change: models.ChangeAdded,
			New:    &b[j-1],
		})
	}
	if i > 0 && (j == 0 || matrix[i][j-1] < matrix[i-1][j]) {
		res := backtrack(matrix, a, b, i-1, j)
		return append(res, models.BlockDiff{
			Change: models.ChangeRemoved,
			Old:    &a[i-1],
		})
	}
	return []models.BlockDiff{}
}

func blocksEqual(a, b *models.Block) bool {
	// For a structural diff, equality should be based on type and content,
	// not UUID (which is generated per parse).
	return a.RawType == b.RawType && a.Name == b.Name && a.Indent == b.Indent
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
