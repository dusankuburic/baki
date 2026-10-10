// Package contract provides a shared test suite that every StorageBackend implementation (Postgres, the
// filesystem backend and the test double in internal/testutil) runs against, so they can't quietly drift apart
// on return-shape semantics ("not found" → nil vs interfaces.ErrNotFound; "empty list" → nil vs non-nil empty
// slice — historically LoadConversation branched frontend nil-checks per deployment) or on store semantics
// (semantics.go). Not a `_test` package: each backend's test file imports it and calls RunSuite (Postgres opt-in
// via DATABASE_URL).
package contract

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"pad-analyzer/internal/storage/interfaces"
)

// Option tunes RunSuite.
type Option func(*suiteConfig)

type suiteConfig struct {
	known map[string]string // scenario name -> why this backend still diverges
}

// KnownDivergence skips scenario name, with the reason, for a backend that does not meet it yet. Only the frozen
// filesystem backend uses it (the test double meets every scenario), and the list may only shrink. Set
// CONTRACT_STRICT=1 to run the listed scenarios anyway and see which now pass. A name that matches no scenario
// fails the suite, so a typo cannot skip anything.
func KnownDivergence(name, reason string) Option {
	return func(c *suiteConfig) { c.known[name] = reason }
}

// RunSuite executes the cross-backend contract scenarios against b. The suite seeds uuid-prefixed
// fixtures, so it is safe to re-run against a backend with existing state.
func RunSuite(t *testing.T, b interfaces.StorageBackend, opts ...Option) {
	t.Helper()
	ctx := context.Background()
	runID := uuid.New().String()[:8]

	cfg := suiteConfig{known: map[string]string{}}
	for _, o := range opts {
		o(&cfg)
	}
	strict := os.Getenv("CONTRACT_STRICT") != ""
	seen := map[string]bool{}
	run := func(name string, fn func(t *testing.T)) {
		seen[name] = true
		t.Run(name, func(t *testing.T) {
			if reason, ok := cfg.known[name]; ok && !strict {
				t.Skipf("known divergence of this backend: %s", reason)
			}
			fn(t)
		})
	}
	defer func() {
		var unknown []string
		for name := range cfg.known {
			if !seen[name] {
				unknown = append(unknown, name)
			}
		}
		sort.Strings(unknown)
		if len(unknown) > 0 {
			t.Errorf("KnownDivergence names no contract scenario: %s", strings.Join(unknown, ", "))
		}
	}()

	runReturnShapeScenarios(ctx, b, run)
	runCreateUserScenarios(ctx, b, runID, run)
	runConversationScenarios(ctx, b, runID, run)
	runSettingsScenarios(ctx, b, runID, run)
	runUserQueryScenarios(ctx, b, runID, run)
	runFlowScenarios(ctx, b, runID, run)
	runFindingStatusScenarios(ctx, b, runID, run)
	runAPITokenScenarios(ctx, b, runID, run)
	runFlowBaselineScenarios(ctx, b, runID, run)
	runGovernanceAlertScenarios(ctx, b, runID, run)
	runFlowWalkScenarios(ctx, b, runID, run)
	runOrgCustomRuleScenarios(ctx, b, runID, run)
	runOrgChannelScenarios(ctx, b, runID, run)
	runCommentScenarios(ctx, b, runID, run)
	runSemanticsScenarios(ctx, b, runID, run)
	runLookupScenarios(ctx, b, runID, run)
	runBugScenarios(ctx, b, runID, run)
	runLifecycleScenarios(ctx, b, runID, run)
	runRuleSetScenarios(ctx, b, runID, run)
	runCustomRuleScenarios(ctx, b, runID, run)
}
