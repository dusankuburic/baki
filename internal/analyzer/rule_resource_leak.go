package analyzer

import (
	"strings"

	"pad-analyzer/internal/models"
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

func (r *ResourceLeakRule) ID() string          { return "resource-leak" }
func (r *ResourceLeakRule) Name() string         { return "Resource opened but never closed" }
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

	// Consider the resource "closed" if any block in the flow:
	//   (a) has a rawType matching the close prefix, AND
	//   (b) references the output variable (either in Variables or in a property value).
	for _, b := range ctx.AllBlocks {
		if !strings.HasPrefix(b.RawType, matched.closePrefix) {
			continue
		}
		if blockReferencesVar(b, outputVar) {
			return nil
		}
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
// or as a bare value / %varName% in any property.
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
