package analyzer

import (
	"regexp"
	"strings"

	"pad-core/models"
)

type HardcodedURLRule struct{}

func (r *HardcodedURLRule) ID() string   { return "hardcoded-url" }
func (r *HardcodedURLRule) Name() string { return "Hardcoded URL" }
func (r *HardcodedURLRule) Description() string {
	return "Hardcoded URLs and API endpoints that should be parameterized for different environments."
}
func (r *HardcodedURLRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *HardcodedURLRule) Category() string                 { return "Portability" }

var (
	// The character class excludes whitespace, quotes, brackets and sentence
	// punctuation (>,;) but NOT "." — dots occur inside every domain/path, so
	// excluding them truncated URLs at the first dot and collapsed distinct
	// URLs to the same dedup key. A trailing "." is trimmed after the match.
	urlPattern     = regexp.MustCompile(`(?i)\b(https?://|ftp://|www\.)[^\s"')\]}>,;]+`)
	padVariableRef = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*%`)
)

func (r *HardcodedURLRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	var findings []models.Finding
	seen := make(map[string]bool)

	for key, val := range block.Properties {
		if padVariableRef.MatchString(val) {
			continue
		}
		lowerKey := strings.ToLower(key)
		if strings.Contains(lowerKey, "url") ||
			strings.Contains(lowerKey, "address") ||
			strings.Contains(lowerKey, "endpoint") ||
			strings.Contains(lowerKey, "host") ||
			strings.Contains(lowerKey, "server") ||
			strings.Contains(lowerKey, "api") ||
			strings.Contains(lowerKey, "web") ||
			strings.Contains(lowerKey, "connection") {
			m := strings.TrimRight(urlPattern.FindString(val), ".")
			if m != "" && !seen[m] {
				seen[m] = true
				findings = append(findings, models.Finding{
					RuleID:      r.ID(),
					Severity:    r.DefaultSeverity(),
					Title:       "Hardcoded URL",
					Description: "Property '" + key + "' contains a hardcoded URL: " + truncate(m, 60),
					BlockID:     block.ID,
					SubflowID:   block.SubflowID,
					Suggestion:  "Store the base URL in a variable so it can be changed per environment without editing the flow.",
					Metadata:    map[string]interface{}{"property": key, "url": m},
				})
			}
		}
	}

	return findings
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&HardcodedURLRule{}) }
