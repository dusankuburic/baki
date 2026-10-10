package interfaces

import (
	"context"

	"pad-core/models"
)

// RuleSetStore resolves the rule sets baselines and health snapshots record (ADR 0016).
type RuleSetStore interface {
	// RuleSetsByDigest returns, per digest, the rule set a baseline or health snapshot records under it; a digest no
	// current record carries is absent; never nil. A system read for the governance scanner: batch reads return
	// digests only, and it resolves those that differ from the analysis' own (callers pinned by
	// TestArch_SystemReadsPinned).
	RuleSetsByDigest(ctx context.Context, digests []string) (map[string]*models.RuleSet, error)
}
