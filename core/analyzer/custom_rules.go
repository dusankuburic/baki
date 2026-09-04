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
	sev    models.Severity
	nameRe *regexp.Regexp
	rawRe  *regexp.Regexp
}

// LoadCustomRules reads a custom-rules JSON file. Invalid entries are
// skipped, but each skip is now REPORTED as a warning (index + rule id +
// reason) — the old silent `continue` meant a typo'd regex or unknown autoFix
// quietly removed a rule the operator believed was enforcing. Callers decide
// where warnings surface (CLI stderr, server startup log).
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
// full CONFIGURATION, not just its ID.
//
// The analysis cache keys on the rule set, and two orgs can legitimately define
// different rules under the same ID (there is no global namespace for
// user-authored rule IDs). Folding only the ID would let org B be served org A's
// cached findings for a same-named, different-behaviour rule.
//
// encoding/json sorts map keys, so PropertyHas/PropertyNot serialise
// deterministically and the digest is stable across processes.
func (r *CustomRule) RuleDigest() string {
	b, err := json.Marshal(r.config)
	if err != nil {
		// Marshalling CustomRuleConfig cannot fail (plain strings and
		// map[string]string). If it somehow did, fall back to a rendering that
		// is still a faithful function of the config rather than to a constant
		// — a constant would silently re-open the cross-tenant collision this
		// method exists to close. Go renders map literals with sorted keys, so
		// this stays deterministic.
		b = []byte(fmt.Sprintf("%#v", r.config))
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
		r.sev = models.Severity(cfg.Severity)
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
	if cfg.AutoFix != "" {
		if !isValidFixType(cfg.AutoFix) {
			return nil, fmt.Errorf("custom rule %q: unknown autoFix %q", cfg.ID, cfg.AutoFix)
		}
	}
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

	if r.config.PropertyHas != nil {
		for key, pattern := range r.config.PropertyHas {
			val, ok := block.Properties[key]
			if !ok {
				return nil
			}
			if !strings.Contains(strings.ToLower(val), strings.ToLower(pattern)) {
				return nil
			}
		}
	}

	if r.config.PropertyNot != nil {
		for key, pattern := range r.config.PropertyNot {
			val, ok := block.Properties[key]
			if ok && strings.Contains(strings.ToLower(val), strings.ToLower(pattern)) {
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
