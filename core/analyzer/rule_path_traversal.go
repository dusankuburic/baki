package analyzer

import (
	"strings"

	"pad-core/models"
)

// PathTraversalRiskRule flags file-system actions that use a VARIABLE path
// (e.g. File.OpenTextFile Path: %UserInput%) — a traversal vector when the
// variable can carry ../ sequences that escape the intended directory
// (read/write of arbitrary files). A static/hardcoded path is NOT flagged
// (covered by the portability rule); only variable paths are.
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

// pathPropertySuffixes — property keys that carry a path, matched by SUFFIX
// (lower-cased) because PAD names them per action: File.WriteText → File,
// File.Delete → Files, File.Copy → FilesToCopy + Destination, Folder.Create →
// FolderPath, Archive.Zip → ArchivePath. The old exact allow-list named none
// of the keys the parser emits for the common actions, so on real flows the
// rule never fired.
var pathPropertySuffixes = []string{"file", "files", "folder", "folders", "path", "paths", "destination", "source", "directory"}

func isPathProperty(key string) bool {
	k := strings.ToLower(key)
	for _, suf := range pathPropertySuffixes {
		if strings.HasSuffix(k, suf) {
			return true
		}
	}
	return false
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
	var keys [16]string
	for _, key := range sortedPropertyKeys(keys[:0], block.Properties) {
		val := block.Properties[key]
		if !isPathProperty(key) {
			continue
		}
		// A %Variable% reference in the path is the traversal vector. A
		// literal path (even with ../) is not a dynamic-traversal risk and is
		// left to the hardcoded-path portability rule; %% is PAD's literal
		// percent escape, not a variable.
		if sqlVarRef.MatchString(val) {
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

func init() {
	registerRule(&PathTraversalRiskRule{}, RuleMeta{
		Confidence:   models.ConfidenceMedium,
		IntroducedIn: 1,
		Revision:     2,
		Stage:        StageStable,
	})
}
