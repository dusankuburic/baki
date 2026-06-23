package analyzer

import (
	"regexp"
	"strings"

	"pad-core/models"
)

type HardcodedFilePathRule struct{}

func (r *HardcodedFilePathRule) ID() string   { return "hardcoded-filepath" }
func (r *HardcodedFilePathRule) Name() string { return "Hardcoded file path" }
func (r *HardcodedFilePathRule) Description() string {
	return "Absolute file paths that break when the flow runs on a different machine."
}
func (r *HardcodedFilePathRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *HardcodedFilePathRule) Category() string                 { return "Portability" }

var (
	winAbsPath  = regexp.MustCompile(`[A-Za-z]:[\\][^\s"']+`)
	unixAbsPath = regexp.MustCompile(`/(home|Users|var|tmp|opt|etc|usr|mnt|root)/[^\s"']+`)
)

func (r *HardcodedFilePathRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	var findings []models.Finding
	seen := make(map[string]bool)

	for key, val := range block.Properties {
		if strings.Contains(val, "%") {
			continue
		}
		lowerKey := strings.ToLower(key)
		if !strings.Contains(lowerKey, "path") &&
			!strings.Contains(lowerKey, "file") &&
			!strings.Contains(lowerKey, "folder") &&
			!strings.Contains(lowerKey, "directory") &&
			!strings.Contains(lowerKey, "source") &&
			!strings.Contains(lowerKey, "destination") &&
			!strings.Contains(lowerKey, "output_") {
			continue
		}

		var matched string
		if m := winAbsPath.FindString(val); m != "" && len(m) > 5 {
			matched = m
		} else if m := unixAbsPath.FindString(val); m != "" && len(m) > 8 {
			matched = m
		}

		if matched != "" && !seen[matched] {
			seen[matched] = true
			findings = append(findings, models.Finding{
				RuleID:      r.ID(),
				Severity:    r.DefaultSeverity(),
				Title:       "Hardcoded file path",
				Description: "Property '" + key + "' contains an absolute path: " + truncate(matched, 60),
				BlockID:     block.ID,
				SubflowID:   block.SubflowID,
				Suggestion:  "Use a relative path or store the base directory in a variable configured per environment.",
				Metadata:    map[string]interface{}{"property": key, "path": matched},
			})
		}
	}

	return findings
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-3]) + "..."
}
