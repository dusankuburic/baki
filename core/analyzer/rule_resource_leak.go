package analyzer

import (
	"strings"

	"pad-core/models"
)

// resourcePair maps an "open" action prefix to the expected "close" action prefix.
type resourcePair struct {
	openPrefix  string
	closePrefix string
	// closeAction is the concrete action named in the suggestion and emitted by
	// the insert-close fixer. Empty means closePrefix is itself a real action.
	closeAction string
	label       string
}

// Each entry covers one resource lifecycle. The open prefix must be specific
// enough not to match unrelated actions; both prefixes are matched with a
// case-insensitive HasPrefix (hasPrefixFold), so partial module names (e.g.
// "Excel.Close") catch all Close variants and exporter casing differences
// ("Ftp." vs "FTP.") do not matter.
var resourcePairs = []resourcePair{
	{openPrefix: "File.OpenTextFile", closePrefix: "File.CloseTextFile", label: "file"},
	{openPrefix: "Database.Connect", closePrefix: "Database.Close", label: "database connection"},
	// Database.OpenSQLConnection / Database.OpenODBCConnection.
	{openPrefix: "Database.Open", closePrefix: "Database.Close", label: "database connection"},
	// FTP.OpenConnection / FTP.OpenSecureConnection.
	{openPrefix: "FTP.Open", closePrefix: "FTP.CloseConnection", label: "FTP connection"},
	{openPrefix: "Excel.LaunchExcel", closePrefix: "Excel.Close", label: "Excel instance"},
	{openPrefix: "Excel.AttachToRunningExcel", closePrefix: "Excel.Close", label: "Excel instance"},
	{openPrefix: "Outlook.Launch", closePrefix: "Outlook.Close", label: "Outlook instance"},
	{openPrefix: "SAP.Login", closePrefix: "SAP.Logout", label: "SAP session"},
	// WebAutomation.LaunchChrome / LaunchEdge / LaunchFirefox / LaunchBrowser,
	// released by CloseWebBrowser (or CloseBrowser / CloseWindow).
	{openPrefix: "WebAutomation.Launch", closePrefix: "WebAutomation.Close", closeAction: "WebAutomation.CloseWebBrowser", label: "web browser instance"},
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// matchResourcePair returns the resource lifecycle opened by rawType, or nil.
// Shared by the rule and the insert-close fixer so they cannot disagree about
// which actions open a resource.
func matchResourcePair(rawType string) *resourcePair {
	for i := range resourcePairs {
		if hasPrefixFold(rawType, resourcePairs[i].openPrefix) {
			return &resourcePairs[i]
		}
	}
	return nil
}

// closeActionName is the action the suggestion/fixer names for the pair.
func (p *resourcePair) closeActionName() string {
	if p.closeAction != "" {
		return p.closeAction
	}
	return p.closePrefix
}

type ResourceLeakRule struct{}

func (r *ResourceLeakRule) ID() string   { return "resource-leak" }
func (r *ResourceLeakRule) Name() string { return "Resource opened but never closed" }
func (r *ResourceLeakRule) Description() string {
	return "Actions that open files, connections, or application instances without a corresponding close action."
}
func (r *ResourceLeakRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *ResourceLeakRule) Category() string                 { return "Reliability" }

func (r *ResourceLeakRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	matched := matchResourcePair(block.RawType)
	if matched == nil {
		return nil
	}

	outputVar := block.Properties["_output"]
	if outputVar == "" {
		return nil
	}

	// Closed if some close-action block references the output variable.
	// ClosedResourceVars precomputes, per close prefix, the variables referenced
	// by matching blocks (see buildClosedResourceVars) — O(1) instead of an
	// O(blocks) scan per open action.
	if closed := ctx.ClosedResourceVars()[matched.closePrefix]; closed[outputVar] {
		return nil
	}

	return []models.Finding{{
		RuleID:   r.ID(),
		Severity: r.DefaultSeverity(),
		Title:    "Resource opened but never closed",
		Description: "This action opens a " + matched.label + " (stored in %" + outputVar +
			"%) but no matching close action was found in the flow.",
		BlockID:   block.ID,
		SubflowID: block.SubflowID,
		Suggestion: "Add a '" + matched.closeActionName() + "' action to release the resource. " +
			"Place it inside an 'On Block Error' handler to guarantee cleanup even on failure.",
		Metadata: map[string]any{"resource": matched.label, "variable": outputVar},
	}}
}

// blockReferencesVar returns true if the block uses varName in its Variables list
// or as a bare value / %varName% in any property. addReferencedVars below is the
// set-building inverse of this predicate, used to precompute closed-resource vars.
func blockReferencesVar(b *models.Block, varName string) bool {
	for _, v := range b.Variables {
		if v == varName {
			return true
		}
	}
	wrapped := "%" + varName + "%"
	for _, v := range b.Properties {
		if v == varName || v == wrapped {
			return true
		}
	}
	return false
}

// addReferencedVars records every variable b references, mirroring
// blockReferencesVar exactly: in b.Variables, or a property value equal to the
// bare name, or equal to "%name%" (add the raw value; strip the %…% wrapper).
func addReferencedVars(set map[string]bool, b *models.Block) {
	for _, v := range b.Variables {
		set[v] = true
	}
	for _, val := range b.Properties {
		set[val] = true
		if len(val) >= 2 && val[0] == '%' && val[len(val)-1] == '%' {
			set[val[1:len(val)-1]] = true
		}
	}
}

// buildClosedResourceVars indexes, per resource close-action prefix, the set of
// variables referenced by any block performing that close, so the rule resolves
// "is this handle closed anywhere?" with an O(1) lookup (was O(opens·blocks)).
func buildClosedResourceVars(ctx *RuleContext) map[string]map[string]bool {
	out := make(map[string]map[string]bool)
	for _, b := range ctx.AllBlocks {
		for i := range resourcePairs {
			cp := resourcePairs[i].closePrefix
			if !hasPrefixFold(b.RawType, cp) {
				continue
			}
			set := out[cp]
			if set == nil {
				set = make(map[string]bool)
				out[cp] = set
			}
			addReferencedVars(set, b)
		}
	}
	return out
}

func init() {
	registerRule(&ResourceLeakRule{}, RuleMeta{
		Confidence:   models.ConfidenceHigh,
		AutoFix:      "insert-close",
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
