package analyzer

import (
	"strings"

	"pad-core/models"
)

// PathTraversalRiskRule flags file-system actions that use a VARIABLE path
// (e.g. File.OpenTextFile Path: %UserInput%) — a path-traversal vector when the
// variable can carry ../ sequences. The existing file-op rules check for missing
// error handlers / hardcoded paths; this one raises the security concern that a
// caller-controlled path can escape the intended directory (read/write of
// arbitrary files). A static/hardcoded path is NOT flagged (covered by the
// portability rule); only variable paths are.
type PathTraversalRiskRule struct{}

func (r *PathTraversalRiskRule) ID() string   { return "path-traversal-risk" }
func (r *PathTraversalRiskRule) Name() string { return "Path traversal risk" }
func (r *PathTraversalRiskRule) Description() string {
	return "File actions that use a variable path. A caller-controlled value containing ../ sequences can escape the intended directory and read or overwrite arbitrary files."
}
func (r *PathTraversalRiskRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *PathTraversalRiskRule) Category() string                 { return "Security" }

// fileActionPrefixes — PAD file-system action families.
var fileActionPrefixes = []string{
	"file.",
	"filesystem.",
	"folder.",
	"archive.",
}

// pathPropertyKeys — property keys carrying the target path.
var pathPropertyKeys = map[string]bool{
	"path":        true,
	"filepath":    true,
	"filename":    true,
	"destination": true,
	"source":      true,
	"directory":   true,
	"folderpath":  true,
	"outputpath":  true,
	"inputpath":   true,
}

func (r *PathTraversalRiskRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	rawLower := strings.ToLower(block.RawType)
	isFile := false
	for _, p := range fileActionPrefixes {
		if strings.HasPrefix(rawLower, p) {
			isFile = true
			break
		}
	}
	if !isFile {
		return nil
	}
	for key, val := range block.Properties {
		if !pathPropertyKeys[strings.ToLower(key)] {
			continue
		}
		// A variable reference in the path is the traversal vector. A literal
		// path (even with ../) is not a dynamic-traversal risk and is left to
		// the hardcoded-path portability rule.
		if strings.Contains(val, "%") {
			return []models.Finding{{
				RuleID:      r.ID(),
				Severity:    r.DefaultSeverity(),
				Title:       "Path traversal risk",
				Description: "File action uses a variable path. A caller-controlled value containing ../ sequences can escape the intended directory and read or overwrite arbitrary files.",
				BlockID:     block.ID,
				SubflowID:   block.SubflowID,
				Suggestion:  "Validate/canonicalize the variable path (resolve and confirm it stays within the allowed root) before opening, writing, or deleting.",
				Metadata:    map[string]interface{}{"property": key},
			}}
		}
	}
	return nil
}

func init() { registerRule(&PathTraversalRiskRule{}) }
