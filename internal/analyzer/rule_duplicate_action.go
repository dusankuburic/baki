package analyzer

import (
	"crypto/md5"
	"encoding/hex"
	"sort"
	"strings"

	"pad-analyzer/internal/models"
)

type DuplicateActionRule struct{}

func (r *DuplicateActionRule) ID() string          { return "duplicate-action" }
func (r *DuplicateActionRule) Name() string         { return "Repeated action pattern" }
func (r *DuplicateActionRule) Description() string  { return "3+ identical actions in sequence (same RawType and similar properties)." }
func (r *DuplicateActionRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *DuplicateActionRule) Category() string     { return "Style" }

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

	myIdx := -1
	for i, s := range siblings {
		if s.ID == block.ID {
			myIdx = i
			break
		}
	}

	sig := blockSignature(block)

	matching := 1
	for i := myIdx + 1; i < len(siblings); i++ {
		if blockSignature(siblings[i]) == sig {
			matching++
		} else {
			break
		}
	}

	for i := myIdx - 1; i >= 0; i-- {
		if blockSignature(siblings[i]) == sig {
			matching++
		} else {
			break
		}
	}

	if matching < minRepeats {
		return nil
	}

	if myIdx > 0 && blockSignature(siblings[myIdx-1]) == sig {
		return nil
	}

	return []models.Finding{{
		RuleID:     r.ID(),
		Severity:   r.DefaultSeverity(),
		Title:      "Repeated action pattern",
		Description: "This action is repeated multiple times in sequence.",
		BlockID:    block.ID,
		SubflowID:  block.SubflowID,
		Suggestion: "Consider extracting these repeated actions into a subflow.",
		Metadata:   map[string]interface{}{"repeatCount": matching},
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

	h := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(h[:])
}
