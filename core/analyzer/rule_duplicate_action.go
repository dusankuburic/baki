package analyzer

import (
	"crypto/md5" // #nosec G501 -- content fingerprint for dedup, not a security primitive
	"encoding/hex"
	"sort"
	"strings"

	"pad-core/models"
)

type DuplicateActionRule struct{}

func (r *DuplicateActionRule) ID() string   { return "duplicate-action" }
func (r *DuplicateActionRule) Name() string { return "Repeated action pattern" }
func (r *DuplicateActionRule) Description() string {
	return "3+ identical actions in sequence (same RawType and similar properties)."
}
func (r *DuplicateActionRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *DuplicateActionRule) Category() string                 { return "Style" }

func (r *DuplicateActionRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	minRepeats := 3
	if ctx.Settings != nil {
		if rc, ok := ctx.Settings.Analysis.Rules[r.ID()]; ok {
			if mr, ok := rc.Options["minRepeats"]; ok {
				if f, ok := mr.(float64); ok && f >= 2 {
					minRepeats = int(f)
				}
			}
		}
	}

	siblings := GetSiblings(ctx, block)
	if len(siblings) < minRepeats {
		return nil
	}

	// BlockIndex already holds this block's position within its sibling group
	// (computed once in collectBlocks), so use it instead of re-scanning.
	myIdx, ok := ctx.BlockIndex[block.ID]
	if !ok {
		return nil
	}

	sig := ctx.blockSig(block)

	// The finding is emitted only on the FIRST block of a run. If the previous
	// sibling shares our signature, we're mid-run — return in O(1) rather than
	// rescanning the entire run for every member (was O(run²) overall, the
	// dominant cost in profiling once MD5 was memoized).
	if myIdx > 0 && ctx.blockSig(siblings[myIdx-1]) == sig {
		return nil
	}

	// Count the forward run length from this run-start block. The backward scan
	// the previous implementation did was always zero here (we're the start),
	// so it is removed; repeatCount is identical.
	matching := 1
	for i := myIdx + 1; i < len(siblings); i++ {
		if ctx.blockSig(siblings[i]) == sig {
			matching++
		} else {
			break
		}
	}

	if matching < minRepeats {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Repeated action pattern",
		Description: "This action is repeated multiple times in sequence.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Consider extracting these repeated actions into a subflow.",
		Metadata:    map[string]any{"repeatCount": matching},
	}}
}

func blockSignature(b *models.Block) string {
	keys := make([]string, 0, len(b.Properties))
	for k := range b.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(b.RawType)
	sb.WriteByte('|')
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(b.Properties[k])
		sb.WriteByte(';')
	}

	h := md5.Sum([]byte(sb.String())) // #nosec G401 -- content fingerprint for dedup, not security
	return hex.EncodeToString(h[:])
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&DuplicateActionRule{}) }
