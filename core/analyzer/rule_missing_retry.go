package analyzer

import (
	"strings"

	"pad-core/models"
)

var transientPrefixes = []string{
	"WebAutomation.",
	"Http.",
	"Ftp.",
	"Database.",
	"Email.",
	"Outlook.",
	"SharePoint.",
	"OneDrive.",
	"ActiveDirectory.",
}

type MissingRetryRule struct{}

func (r *MissingRetryRule) ID() string   { return "missing-retry" }
func (r *MissingRetryRule) Name() string { return "Transient operation without retry" }
func (r *MissingRetryRule) Description() string {
	return "Network/external service actions prone to transient failures that lack retry logic."
}
func (r *MissingRetryRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *MissingRetryRule) Category() string                 { return "Reliability" }

func (r *MissingRetryRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeAction {
		return nil
	}

	if !isTransientOperation(block.RawType) {
		return nil
	}

	if HasErrorHandlerAncestor(ctx, block) {
		return nil
	}

	if isInsideRetryLoop(block, ctx) {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Transient operation without retry",
		Description: "This network/external service action is prone to transient failures (timeouts, rate limits, connection resets) but has no retry mechanism or error handler.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Wrap this action in a loop with a retry counter and delay, or add an 'On Block Error' handler that implements retry logic.",
		AutoFixHint: "Wrap the action: SET %RetryCount% = 0 > LOOP while %RetryCount% < 3 > [action] > ON ERROR: Wait 2s, SET %RetryCount% = %RetryCount% + 1 > END LOOP.",
		Metadata:    map[string]interface{}{"rawType": block.RawType},
	}}
}

func isTransientOperation(rawType string) bool {
	for _, prefix := range transientPrefixes {
		if strings.HasPrefix(rawType, prefix) {
			return true
		}
	}
	return false
}

func isInsideRetryLoop(block *models.Block, ctx *RuleContext) bool {
	cur := block.ID
	visited := make(map[string]bool)
	for {
		pid, ok := ctx.ParentMap[cur]
		if !ok {
			return false
		}
		if visited[pid] {
			return false
		}
		visited[pid] = true
		parent := ctx.AllBlocks[pid]
		if parent == nil {
			return false
		}
		if parent.Type == models.BlockTypeLoop {
			nameLower := strings.ToLower(parent.Name)
			hasRetry := strings.Contains(nameLower, "retry") ||
				strings.Contains(nameLower, "attempt")
			// range over a nil map is safe; no guard needed.
			for k, v := range parent.Properties {
				kl := strings.ToLower(k)
				if (strings.Contains(kl, "retry") || strings.Contains(kl, "attempt")) && v != "" {
					hasRetry = true
				}
			}
			if hasRetry {
				return true
			}
			// Not a retry loop — keep walking up to check outer loops/ancestors.
		}
		cur = pid
	}
}
