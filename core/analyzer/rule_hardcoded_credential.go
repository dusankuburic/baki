package analyzer

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"pad-core/models"
)

type HardcodedCredentialRule struct{}

func (r *HardcodedCredentialRule) ID() string   { return "hardcoded-credential" }
func (r *HardcodedCredentialRule) Name() string { return "Hardcoded credential detected" }
func (r *HardcodedCredentialRule) Description() string {
	return "String values matching credential patterns, and credential-typed action properties (Password, ApiKey, Token, …) set to a literal."
}
func (r *HardcodedCredentialRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *HardcodedCredentialRule) Category() string                 { return "Security" }

// Patterns use [^%"'\s] for the secret value portion to exclude PAD variable
// references (%VarName%). sensitiveVarName matches names — and, since the
// key-aware check below, action property KEYS — likely to hold credentials.
var sensitiveVarName = regexp.MustCompile(`(?i)(password|passwd|pwd|passphrase|api[_\-]?key|token|secret|credential|auth[_\-]?key|bearer|access[_\-]?key)`)

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)password\s*[:=]\s*["'][^%"'\s]{4,}["']`),
	// Unquoted `password=…;` as it appears inside a connection string; the
	// parser strips the outer quotes, so the quoted form above never saw it.
	regexp.MustCompile(`(?i)\b(?:password|passwd|pwd)\s*=\s*[^%"'\s;]{4,}\s*(?:;|$)`),
	regexp.MustCompile(`(?i)api[_-]?key\s*[:=]\s*["'][^%"'\s]{16,}["']`),
	regexp.MustCompile(`(?i)token\s*[:=]\s*["'][^%"'\s]{16,}["']`),
	regexp.MustCompile(`(?i)secret\s*[:=]\s*["'][^%"'\s]{8,}["']`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
}

// credentialActionProperties names, per action, the property keys that carry
// a secret by the action's own contract but whose NAME does not say so (the
// generic key check cannot see them); seeded from the AI scrubber's
// sensitiveActions. Keys named Password/ConnectionString are already covered.
var credentialActionProperties = map[string][]string{
	"Cryptography.DecryptTextWithAES":     {"Key"},
	"Cryptography.EncryptTextWithAES":     {"Key"},
	"Cryptography.DecryptFromFileWithAES": {"Key"},
	"Cryptography.EncryptToFileWithAES":   {"Key"},
	"Cryptography.HashText":               {"Key"},
	"Cryptography.HashFromFile":           {"Key"},
}

func (r *HardcodedCredentialRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	var findings []models.Finding
	// Lazily allocated: most blocks flag nothing, and this Check runs for
	// every (block, rule) pair in the walk. Reads on the nil map are false,
	// which is exactly the "not flagged yet" answer.
	var patternFlagged map[string]bool
	flag := func(key string) {
		if patternFlagged == nil {
			patternFlagged = map[string]bool{}
		}
		patternFlagged[key] = true
	}

	var keys [16]string
	for _, key := range sortedPropertyKeys(keys[:0], block.Properties) {
		val := block.Properties[key]
		for _, pat := range credentialPatterns {
			if pat.MatchString(val) {
				flag(key)
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

		// Key-aware check: the property is a credential by the action's contract
		// (`Email.SendEmailSMTP … Password: 'hunter2'`), whatever the value looks
		// like — the value patterns above only inspect the VALUE, so a plain
		// password under a Password: key was never flagged.
		if !patternFlagged[key] && isCredentialProperty(block, key, val) {
			flag(key)
			findings = append(findings, models.Finding{
				RuleID:   r.ID(),
				Severity: r.DefaultSeverity(),
				Title:    "Credential property set to literal value",
				Description: "Property '" + key + "' of '" + block.RawType + "' carries a credential " +
					"and is set to a literal value instead of a variable.",
				BlockID:     block.ID,
				SubflowID:   block.SubflowID,
				Suggestion:  "Move this credential to a secured variable or vault. Hardcoded secrets in flows are a security risk.",
				AutoFixHint: "Replace the literal value with a %InputVariable% declared as Sensitive in the flow's input properties, or retrieve it at runtime using the 'Get password from CyberArk / Windows Credential Manager' action.",
				Metadata:    map[string]interface{}{"property": key},
			})
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

// isCredentialProperty reports whether property key on an ACTION block is a
// credential-typed property holding a literal (non-%Var%) value worth flagging:
// by name (sensitiveVarName: Password, ApiKey, Token, Secret, …) or by the
// action's contract (credentialActionProperties). Parser-synthesised keys
// (`_output`, `_var`, `_value`) are skipped — `=> %Password%` names the OUTPUT
// variable and the SET-variable form is handled separately above. Placeholders
// (`****`, `True`, a bare number) and anything under four chars are not secrets.
func isCredentialProperty(block *models.Block, key, val string) bool {
	if block.Type != models.BlockTypeAction || strings.HasPrefix(key, "_") {
		return false
	}
	if strings.Contains(val, "%") {
		return false
	}
	byName := sensitiveVarName.MatchString(key)
	byContract := false
	for _, k := range credentialActionProperties[block.RawType] {
		if strings.EqualFold(k, key) {
			byContract = true
			break
		}
	}
	if !byName && !byContract {
		return false
	}
	bare := strings.TrimSpace(strings.Trim(strings.TrimSpace(val), "'\""))
	if len(bare) < 4 || isCredentialPlaceholder(bare) {
		return false
	}
	return true
}

// isCredentialPlaceholder recognises values that sit in a credential property
// without being a credential: a masked value, a boolean/null, a number, or an
// angle-bracket template such as <password>.
func isCredentialPlaceholder(v string) bool {
	if strings.Trim(v, "*•xX·") == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "true", "false", "null", "none", "nil":
		return true
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return true
	}
	if strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">") {
		return true
	}
	return false
}

// isHighEntropySecret flags long random-looking alphanumeric literals. The
// thresholds are deliberately conservative: hex digests top out at 4.0 bits so
// they never match, and mixed-case Base62 identifiers (file/record IDs,
// typically 22-43 chars) fall under the 48-char floor — the main false-positive
// source. Shorter labeled secrets are still caught by credentialPatterns.
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
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
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
	// total must be the rune count, not the byte length: freq counts runes, so
	// dividing by byte length under-states entropy for multibyte UTF-8.
	total := float64(len([]rune(s)))
	for _, count := range freq {
		p := count / total
		if p > 0 {
			entropy -= p * math.Log2(p)
		}
	}
	return entropy
}

func init() {
	registerRule(&HardcodedCredentialRule{}, RuleMeta{
		Confidence:   models.ConfidenceHigh,
		AutoFix:      "replace-with-variable",
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
