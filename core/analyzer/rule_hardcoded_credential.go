package analyzer

import (
	"math"
	"regexp"
	"strings"

	"pad-core/models"
)

type HardcodedCredentialRule struct{}

func (r *HardcodedCredentialRule) ID() string          { return "hardcoded-credential" }
func (r *HardcodedCredentialRule) Name() string         { return "Hardcoded credential detected" }
func (r *HardcodedCredentialRule) Description() string  { return "String values matching credential patterns." }
func (r *HardcodedCredentialRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *HardcodedCredentialRule) Category() string     { return "Security" }

// Patterns use [^%"'\s] for the secret value portion to exclude PAD variable
// references (%VarName%) which are non-whitespace but not literal secrets.
// sensitiveVarName matches variable names that are likely to hold credentials.
var sensitiveVarName = regexp.MustCompile(`(?i)(password|passwd|pwd|api[_\-]?key|token|secret|credential|auth[_\-]?key|bearer|access[_\-]?key)`)

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)password\s*[:=]\s*["'][^%"'\s]{4,}["']`),
	regexp.MustCompile(`(?i)api[_-]?key\s*[:=]\s*["'][^%"'\s]{16,}["']`),
	regexp.MustCompile(`(?i)token\s*[:=]\s*["'][^%"'\s]{16,}["']`),
	regexp.MustCompile(`(?i)secret\s*[:=]\s*["'][^%"'\s]{8,}["']`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
}

func (r *HardcodedCredentialRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	var findings []models.Finding
	patternFlagged := make(map[string]bool)

	for key, val := range block.Properties {
		for _, pat := range credentialPatterns {
			if pat.MatchString(val) {
				patternFlagged[key] = true
				findings = append(findings, models.Finding{
					RuleID:      r.ID(),
					Severity:    r.DefaultSeverity(),
					Title:       "Hardcoded credential detected",
					Description: "A property value matches a known credential pattern.",
					BlockID:     block.ID,
					SubflowID:   block.SubflowID,
					Suggestion:  "Move this credential to a secured variable or vault. Hardcoded secrets in flows are a security risk.",
					AutoFixHint: "Replace the literal value with a %InputVariable% declared as Sensitive in the flow's input properties, or retrieve it at runtime using the 'Get password from CyberArk / Windows Credential Manager' action.",
					Metadata:    map[string]interface{}{"property": key},
				})
				break
			}
		}

		// Skip entropy check for properties already flagged by a pattern to avoid
		// duplicate findings on the same property.
		if !patternFlagged[key] && isHighEntropySecret(val) {
			findings = append(findings, models.Finding{
				RuleID:      r.ID(),
				Severity:    r.DefaultSeverity(),
				Title:       "High-entropy string detected",
				Description: "A property value has high Shannon entropy, suggesting a hardcoded secret or key.",
				BlockID:     block.ID,
				SubflowID:   block.SubflowID,
				Suggestion:  "Move this credential to a secured variable or vault. Hardcoded secrets in flows are a security risk.",
				Metadata:    map[string]interface{}{"property": key},
			})
		}
	}

	// Extra check: SET-variable blocks where the variable NAME itself looks like a
	// credential and the VALUE is a string literal (not a %variable% reference).
	if block.Type == models.BlockTypeVariable {
		varName := block.Properties["_var"]
		if varName == "" {
			varName = block.Properties["_output"]
		}
		varValue := block.Properties["_value"]

		if varName != "" && varValue != "" &&
			sensitiveVarName.MatchString(varName) &&
			!strings.Contains(varValue, "%") {
			bare := strings.Trim(varValue, "'\"")
			if len(bare) >= 4 {
				findings = append(findings, models.Finding{
					RuleID:   r.ID(),
					Severity: r.DefaultSeverity(),
					Title:    "Credential variable set to literal value",
					Description: "Variable '" + varName + "' has a name that suggests it holds a credential " +
						"and is assigned a literal string value.",
					BlockID:   block.ID,
					SubflowID: block.SubflowID,
					Suggestion: "Store credentials in a secured vault, Windows Credential Manager, or use a " +
						"PAD input variable instead of a literal string in the flow.",
					Metadata: map[string]interface{}{"variable": varName},
				})
			}
		}
	}

	return findings
}

// isHighEntropySecret flags long random-looking alphanumeric literals. The
// thresholds are deliberately conservative: hex digests top out at 4.0 bits
// so they never match, and mixed-case Base62 identifiers (file/record IDs,
// typically 22-43 chars) fall under the 48-char floor — those were the main
// false-positive source. Labeled secrets shorter than this are still caught
// by the credentialPatterns regexes above.
func isHighEntropySecret(s string) bool {
	if len(s) < 48 {
		return false
	}
	if !isAlphanumeric(s) {
		return false
	}
	return shannonEntropy(s) > 5.0
}

func isAlphanumeric(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	freq := make(map[rune]float64)
	for _, r := range s {
		freq[r]++
	}
	var entropy float64
	total := float64(len(s))
	for _, count := range freq {
		p := count / total
		if p > 0 {
			entropy -= p * math.Log2(p)
		}
	}
	return entropy
}
