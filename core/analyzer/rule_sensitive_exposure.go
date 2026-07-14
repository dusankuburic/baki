package analyzer

import (
	"regexp"
	"strings"

	"pad-core/models"
)

var sensitiveVarPattern = regexp.MustCompile(`(?i)(password|passwd|pwd|api[_\-]?key|token|secret|credential|auth[_\-]?key|bearer|access[_\-]?key|private[_\-]?key)`)

var sinkPrefixes = []struct {
	prefix   string
	sinkType string
}{
	{"File.Write", "file"},
	{"File.AppendText", "file"},
	{"File.Create", "file"},
	{"Text.WriteTo", "file"},
	{"Display.ShowMessage", "UI message"},
	{"Display.ShowCustomDialog", "UI dialog"},
	{"Text.Display", "UI"},
	{"Logger.", "log"},
	{"System.RunApplication", "process"},
	{"HTTPClient.Invoke", "HTTP request"},
}

type SensitiveDataExposureRule struct{}

func (r *SensitiveDataExposureRule) ID() string   { return "sensitive-exposure" }
func (r *SensitiveDataExposureRule) Name() string { return "Sensitive data exposure" }
func (r *SensitiveDataExposureRule) Description() string {
	return "Variables with sensitive names (passwords, tokens, keys) written to files, logs, or displayed in UI."
}
func (r *SensitiveDataExposureRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *SensitiveDataExposureRule) Category() string                 { return "Security" }

func (r *SensitiveDataExposureRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeAction {
		return nil
	}

	sink := findSink(block.RawType)
	if sink == "" {
		return nil
	}

	var findings []models.Finding
	seen := make(map[string]bool)

	for _, vname := range block.Variables {
		if !sensitiveVarPattern.MatchString(vname) {
			continue
		}
		if seen[vname] {
			continue
		}
		seen[vname] = true

		findings = append(findings, models.Finding{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Sensitive variable in " + sink + " action",
			Description: "Variable '" + vname + "' appears to hold sensitive data and is passed to a " + sink + " action, which could expose credentials.",
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Avoid writing sensitive variables to files, logs, or displaying them. Use secure credential storage instead.",
			Metadata:    map[string]interface{}{"variable": vname, "sink": sink},
		})
	}

	return findings
}

func findSink(rawType string) string {
	for _, s := range sinkPrefixes {
		if strings.HasPrefix(rawType, s.prefix) {
			return s.sinkType
		}
	}
	return ""
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&SensitiveDataExposureRule{}) }
