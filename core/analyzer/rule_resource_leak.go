package analyzer

import (
	"strings"

	"pad-core/models"
)

// resourcePair maps an "open" action prefix to the expected "close" action prefix.
type resourcePair struct {
	openPrefix  string
	closePrefix string
	label       string
}

// Each entry covers one resource lifecycle. The open prefix must be specific enough
// that it doesn't match unrelated actions; the close prefix is matched as a HasPrefix
// so partial module names (e.g. "Excel.Close") catch all Close variants.
var resourcePairs = []resourcePair{
	{"File.OpenTextFile", "File.CloseTextFile", "file"},
	{"Database.Connect", "Database.Close", "database connection"},
	{"Excel.LaunchExcel", "Excel.Close", "Excel instance"},
	{"Excel.AttachToRunningExcel", "Excel.Close", "Excel instance"},
	{"Outlook.Launch", "Outlook.Close", "Outlook instance"},
	{"SAP.Login", "SAP.Logout", "SAP session"},
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
	var matched *resourcePair
	for i := range resourcePairs {
		if strings.HasPrefix(block.RawType, resourcePairs[i].openPrefix) {
			matched = &resourcePairs[i]
			break
		}
	}
	if matched == nil {
		return nil
	}

	outputVar := block.Properties["_output"]
	if outputVar == "" {
		return nil
	}

	// The resource is "closed" if some close-action block references the output
	// variable. ClosedResourceVars precomputes, per close prefix, the set of all
	// variables referenced by matching blocks (see buildClosedResourceVars), so
	// this is O(1) instead of an O(blocks) scan per open action.
	if closed := ctx.ClosedResourceVars[matched.closePrefix]; closed[outputVar] {
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
		Suggestion: "Add a '" + matched.closePrefix + "' action to release the resource. " +
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

// addReferencedVars records every variable that block b references into set,
// mirroring blockReferencesVar exactly: a var counts as referenced if it appears
// in b.Variables, or a property value equals the bare name, or a property value
// equals "%name%". Adding the raw value covers the bare-name case; stripping the
// %…% wrapper covers the wrapped case.
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
// variables referenced by any block performing that close. The resource-leak
// rule then resolves "is this handle closed anywhere?" with an O(1) set lookup
// instead of scanning every block for each open action (was O(opens·blocks)).
func buildClosedResourceVars(ctx *RuleContext) map[string]map[string]bool {
	out := make(map[string]map[string]bool)
	for _, b := range ctx.AllBlocks {
		for i := range resourcePairs {
			cp := resourcePairs[i].closePrefix
			if !strings.HasPrefix(b.RawType, cp) {
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

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&ResourceLeakRule{}) }
