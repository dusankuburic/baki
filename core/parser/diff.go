package parser

import (
	"maps"

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

	for _, newSf := range newDoc.Subflows {
		if oldSf, ok := oldSubflows[newSf.Name]; ok {
			diff.Subflows = append(diff.Subflows, diffSubflow(oldSf, &newSf))
		} else {
			diff.Subflows = append(diff.Subflows, models.SubflowDiff{
				Name:   newSf.Name,
				Change: models.ChangeAdded,
				Blocks: wrapBlocksAsDiff(newSf.Blocks, models.ChangeAdded),
			})
		}
	}

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

		// #nosec G602 -- i is always a valid index (from `range blocks`).
		if len(blocks[i].Children) > 0 {
			res = append(res, wrapBlocksAsDiff(blocks[i].Children, change)...) // #nosec G602
		}
	}
	return res
}

// Diff size caps. The LCS matrix is O(m×n) memory and backtrack recurses O(m+n)
// deep, so a pathologically large subflow could exhaust memory or overflow the
// stack; above these bounds we fall back to a coarse, linear diff.
const (
	maxDiffBlocksPerSide = 5000
	maxDiffCells         = 4_000_000
)

// diffTooLarge reports whether an m×n LCS would exceed the memory/recursion
// budget. The cell-count check is only reached when both sides are already
// within maxDiffBlocksPerSide, so m*n cannot overflow.
func diffTooLarge(m, n int) bool {
	return m > maxDiffBlocksPerSide || n > maxDiffBlocksPerSide || m*n > maxDiffCells
}

func diffSubflow(oldSf, newSf *models.Subflow) models.SubflowDiff {
	oldBlocks := flattenBlocks(oldSf.Blocks)
	newBlocks := flattenBlocks(newSf.Blocks)

	var blocks []models.BlockDiff
	if diffTooLarge(len(oldBlocks), len(newBlocks)) {
		blocks = coarseBlockDiff(oldBlocks, newBlocks)
	} else {
		matrix := lcs(oldBlocks, newBlocks)
		blocks = backtrack(matrix, oldBlocks, newBlocks, len(oldBlocks), len(newBlocks))
	}

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

// coarseBlockDiff is the linear fallback when an exact LCS would exceed the
// size caps: O(m+n) time and memory, no recursion, but it cannot detect
// moves/insertions. A fast path handles "large subflow, nothing changed" exactly.
func coarseBlockDiff(oldBlocks, newBlocks []models.Block) []models.BlockDiff {
	if len(oldBlocks) == len(newBlocks) {
		identical := true
		for i := range oldBlocks {
			if !blocksEqual(&oldBlocks[i], &newBlocks[i]) {
				identical = false
				break
			}
		}
		if identical {
			out := make([]models.BlockDiff, len(newBlocks))
			for i := range newBlocks {
				out[i] = models.BlockDiff{Change: models.ChangeNone, Old: &oldBlocks[i], New: &newBlocks[i]}
			}
			return out
		}
	}
	out := make([]models.BlockDiff, 0, len(oldBlocks)+len(newBlocks))
	for i := range oldBlocks {
		out = append(out, models.BlockDiff{Change: models.ChangeRemoved, Old: &oldBlocks[i]})
	}
	for i := range newBlocks {
		out = append(out, models.BlockDiff{Change: models.ChangeAdded, New: &newBlocks[i]})
	}
	return out
}

func blocksEqual(a, b *models.Block) bool {
	// Structural equality ignores the per-parse UUID but includes Properties:
	// a config-only edit (changed Url:, Timeout:, …) leaves RawType/Name/Indent
	// identical, so without Properties the LCS would hide the edit. maps.Equal
	// is exact; synthetic keys like _output derive deterministically from the
	// source, so they compare stably.
	return a.RawType == b.RawType && a.Name == b.Name && a.Indent == b.Indent &&
		maps.Equal(a.Properties, b.Properties)
}
