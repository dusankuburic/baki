package analyzer

import (
	"strings"

	"pad-core/models"
)

var timeoutRequiredPrefixes = []string{
	"WebAutomation.",
	"UIAutomation.",
	"HTTPClient.",
	"FTP.",
	"Database.",
	"Outlook.SendEmail",
	"Email.SendEmail",
	"SharePoint.",
	"OneDrive.",
}

// timeoutRequiredPrefixesLower mirrors timeoutRequiredPrefixes lowercased
// once at package init, so requiresTimeout does zero allocations per block
// (it previously ToLower'd every prefix on every action).
var timeoutRequiredPrefixesLower = func() []string {
	out := make([]string, len(timeoutRequiredPrefixes))
	for i, p := range timeoutRequiredPrefixes {
		out[i] = strings.ToLower(p)
	}
	return out
}()

type MissingTimeoutRule struct{}

func (r *MissingTimeoutRule) ID() string   { return "missing-timeout" }
func (r *MissingTimeoutRule) Name() string { return "Network operation without timeout" }
func (r *MissingTimeoutRule) Description() string {
	return "Network/UI automation actions that may hang because no explicit timeout is configured."
}
func (r *MissingTimeoutRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *MissingTimeoutRule) Category() string                 { return "Reliability" }

func (r *MissingTimeoutRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeAction {
		return nil
	}

	if !requiresTimeout(block.RawType) {
		return nil
	}

	if hasTimeoutConfigured(block) {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Network operation without timeout",
		Description: "This network/UI automation action has no explicit timeout configured. If the target is unresponsive, the flow will hang indefinitely.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Set an explicit timeout value in the action's properties. For web automation, use 'Wait for element' with a timeout instead of relying on default behavior.",
		Metadata:    map[string]interface{}{"rawType": block.RawType},
	}}
}

func requiresTimeout(rawType string) bool {
	rt := strings.ToLower(rawType)
	for _, prefix := range timeoutRequiredPrefixesLower {
		if strings.HasPrefix(rt, prefix) {
			return true
		}
	}
	return false
}

func hasTimeoutConfigured(block *models.Block) bool {
	if block.Properties == nil {
		return false
	}

	// Single pass: the substring check subsumes the explicit key list this
	// used to iterate (timeout, timeoutInSeconds, connectionTimeout, ... all
	// contain "timeout"), so one scan over Properties suffices.
	for k, v := range block.Properties {
		kl := strings.ToLower(k)
		if (strings.Contains(kl, "timeout") || strings.Contains(kl, "wait")) && v != "" && v != "0" {
			return true
		}
	}

	return false
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&MissingTimeoutRule{}) }
