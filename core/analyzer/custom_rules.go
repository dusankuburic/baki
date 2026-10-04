package analyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"pad-core/models"
)

type CustomRuleConfig struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Severity     string            `json:"severity"`
	Category     string            `json:"category"`
	RawTypeMatch string            `json:"rawTypeMatch"`
	NameMatch    string            `json:"nameMatch"`
	PropertyHas  map[string]string `json:"propertyHas"`
	PropertyNot  map[string]string `json:"propertyNot"`
	TypeMatch    string            `json:"typeMatch"`
	Suggestion   string            `json:"suggestion"`
	AutoFix      string            `json:"autoFix,omitempty"`
}

type CustomRule struct {
	config CustomRuleConfig
	// digest is the compile-time RuleDigest of config: ruleSetDigest folds it
	// into the analysis cache key on every call, and re-marshalling the config
	// per call was the dominant cost of that key for custom rules.
	digest string
	sev    models.Severity
	nameRe *regexp.Regexp
	rawRe  *regexp.Regexp
	// Case-insensitive contains matching pre-lowers the PATTERNS once here
	// instead of per property per block in Check; the value half still has
	// to be lowered per look (it varies with every block).
	propHasLower map[string]string
	propNotLower map[string]string
}

// LoadCustomRules reads a custom-rules JSON file. Invalid entries are
// skipped with a REPORTED warning (index + rule id + reason) — the old
// silent continue meant a typo'd regex quietly removed a rule the operator
// believed was enforcing. Callers decide where warnings surface.
func LoadCustomRules(path string) ([]Rule, []string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- custom-rules path is operator-configured
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	var configs []CustomRuleConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil, nil, err
	}

	var rules []Rule
	var warnings []string
	for i, cfg := range configs {
		r, cerr := NewCustomRule(cfg)
		if cerr != nil {
			id := cfg.ID
			if id == "" {
				id = fmt.Sprintf("#%d (no id)", i)
			}
			// NewCustomRule's regexp errors don't carry the id — prefix it so
			// every warning names the skipped rule unambiguously.
			warnings = append(warnings, fmt.Sprintf("%s: entry %d (rule %q) skipped: %v", filepath.Base(path), i, id, cerr))
			continue
		}
		rules = append(rules, r)
	}
	return rules, warnings, nil
}

// RuleDigest implements analyzer.RuleDigester: it identifies this rule by its
// full CONFIGURATION, not just its ID — two orgs can legitimately define
// different rules under the same ID, and folding only the ID would let org B
// be served org A's cached findings. encoding/json sorts map keys, so the
// digest is stable across processes.
func (r *CustomRule) RuleDigest() string { return r.digest }

// customRuleDigest computes the digest once, at compile time (see the
// digest field).
func customRuleDigest(cfg CustomRuleConfig) string {
	b, err := json.Marshal(cfg)
	if err != nil {
		// Marshalling CustomRuleConfig cannot fail. If it somehow did, fall
		// back to a rendering that is still a faithful function of the config
		// — a constant would re-open the cross-tenant collision this digest
		// closes (Go renders map literals with sorted keys, so it stays
		// deterministic).
		b = []byte(fmt.Sprintf("%#v", cfg))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func NewCustomRule(cfg CustomRuleConfig) (*CustomRule, error) {
	r := &CustomRule{
		config: cfg,
		sev:    models.SeverityWarning,
	}
	if cfg.Severity != "" {
		// Validated here because this is where an untrusted string becomes a
		// Severity (from .bakirc/-custom-rules JSON and POST /api/orgs/{id}/rules).
		// An unrecognised value used to be accepted verbatim and then silently
		// dropped by every downstream consumer — the finding vanished from
		// reports, became a PASSING JUnit testcase and could never fail the gate.
		sev := models.Severity(cfg.Severity)
		if !sev.Valid() {
			return nil, fmt.Errorf("custom rule %q: invalid severity %q (must be one of %v)",
				cfg.ID, cfg.Severity, models.ValidSeverities())
		}
		r.sev = sev
	}
	if cfg.NameMatch != "" {
		re, err := regexp.Compile("(?i)" + cfg.NameMatch)
		if err != nil {
			return nil, err
		}
		r.nameRe = re
	}
	if cfg.RawTypeMatch != "" {
		re, err := regexp.Compile("(?i)" + cfg.RawTypeMatch)
		if err != nil {
			return nil, err
		}
		r.rawRe = re
	}
	lower := func(m map[string]string) map[string]string {
		if len(m) == 0 {
			return nil
		}
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = strings.ToLower(v)
		}
		return out
	}
	r.propHasLower = lower(cfg.PropertyHas)
	r.propNotLower = lower(cfg.PropertyNot)
	if cfg.AutoFix != "" {
		if !isValidFixType(cfg.AutoFix) {
			return nil, fmt.Errorf("custom rule %q: unknown autoFix %q", cfg.ID, cfg.AutoFix)
		}
	}
	r.digest = customRuleDigest(cfg)
	return r, nil
}

func (r *CustomRule) ID() string                       { return r.config.ID }
func (r *CustomRule) Name() string                     { return r.config.Name }
func (r *CustomRule) Description() string              { return r.config.Description }
func (r *CustomRule) DefaultSeverity() models.Severity { return r.sev }
func (r *CustomRule) Category() string                 { return r.config.Category }

func (r *CustomRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if r.config.TypeMatch != "" {
		if !strings.EqualFold(string(block.Type), r.config.TypeMatch) {
			return nil
		}
	}

	if r.rawRe != nil && !r.rawRe.MatchString(block.RawType) {
		return nil
	}

	if r.nameRe != nil && !r.nameRe.MatchString(block.Name) {
		return nil
	}

	if r.propHasLower != nil {
		for key, pattern := range r.propHasLower {
			val, ok := block.Properties[key]
			if !ok {
				return nil
			}
			if !strings.Contains(strings.ToLower(val), pattern) {
				return nil
			}
		}
	}

	if r.propNotLower != nil {
		for key, pattern := range r.propNotLower {
			val, ok := block.Properties[key]
			if ok && strings.Contains(strings.ToLower(val), pattern) {
				return nil
			}
		}
	}

	suggestion := r.config.Suggestion
	if suggestion == "" {
		suggestion = "Review this block for potential issues."
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       r.config.Name,
		Description: r.config.Description,
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  suggestion,
		Category:    r.config.Category,
		AutoFix:     r.config.AutoFix,
	}}
}

// isValidFixType reports whether fixType is a recognized auto-fixer name
// (matches the switch cases in PatchForFix). Used to validate CustomRuleConfig.
func isValidFixType(fixType string) bool {
	switch fixType {
	case "wrap-error-handler", "insert-close", "set-timeout", "insert-delay",
		"insert-delay-in-loop", "insert-handler-log", "init-variable",
		"insert-error-log", "replace-with-variable", "wrap-in-retry",
		"insert-exit-condition", "remove-block", "parameterize-sql",
		"append-output", "mask-sensitive-variable", "insert-default",
		"upgrade-to-https", "sanitize-command-vars", "suppress":
		return true
	default:
		return false
	}
}
