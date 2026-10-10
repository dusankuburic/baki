package analyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"pad-core/models"
)

// CustomRuleConfig is a user-authored rule as JSON: org rows, rules files, .bakirc.json customRulesInline and the
// API bodies all carry it. Its format and versioning rules are ADR 0015 (custom_rule_schema.go).
type CustomRuleConfig struct {
	// Kind and SchemaVersion are omitted when zero (a missing kind is block, a missing version 1), so a legacy
	// rule marshals to the bytes it always did; EncodeCustomRule writes them out.
	Kind          CustomRuleKind    `json:"kind,omitempty"`
	SchemaVersion int               `json:"schemaVersion,omitempty"`
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Severity      string            `json:"severity"`
	Category      string            `json:"category"`
	RawTypeMatch  string            `json:"rawTypeMatch"`
	NameMatch     string            `json:"nameMatch"`
	PropertyHas   map[string]string `json:"propertyHas"`
	PropertyNot   map[string]string `json:"propertyNot"`
	TypeMatch     string            `json:"typeMatch"`
	Suggestion    string            `json:"suggestion"`
	AutoFix       string            `json:"autoFix,omitempty"`

	// unknown holds the decoded top-level keys this build does not know, with their raw values. UnmarshalJSON
	// sets it; it never marshals and never enters a digest.
	unknown map[string]json.RawMessage
}

type CustomRule struct {
	config CustomRuleConfig
	// digest is the compile-time RuleDigest of config: ProfileDigest folds it
	// into the analysis cache key on every call, and re-marshalling the config
	// per call was the dominant cost of that key for custom rules.
	digest string
	// matchDigest is MatchDigest, computed once like digest.
	matchDigest string
	sev         models.Severity
	nameRe      *regexp.Regexp
	rawRe       *regexp.Regexp
	// Case-insensitive contains matching pre-lowers the PATTERNS once here
	// instead of per property per block in Check; the value half still has
	// to be lowered per look (it varies with every block).
	propHasLower map[string]string
	propNotLower map[string]string
}

// LoadCustomRuleFile reads a rules file (any shape DecodeCustomRuleDocument reads) and compiles it tolerantly. A
// missing or unreadable file, malformed JSON and a JSON type error in any entry are the error, as when the file
// was one json.Unmarshal; every other problem is in problems with its entry's index, and an entry with an error
// (an unknown kind or schemaVersion included, even one whose fields do not decode) is skipped.
func LoadCustomRuleFile(path string) ([]Rule, []*CustomRuleError, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- custom-rules path is operator-configured
	if err != nil {
		return nil, nil, err
	}
	return compileCustomRuleDocument(data)
}

func compileCustomRuleDocument(data []byte) ([]Rule, []*CustomRuleError, error) {
	entries, err := DecodeCustomRuleDocument(data)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.Err != nil && !e.Err.IsNewerRelease() {
			return nil, nil, e.Err
		}
	}
	var rules []Rule
	var problems []*CustomRuleError
	for _, e := range entries {
		if e.Err != nil {
			problems = append(problems, e.Err)
			continue
		}
		r, warnings, cerr := CompileCustomRule(e.Config, CustomRuleTolerant)
		for _, w := range warnings {
			w.Index = e.Index
			problems = append(problems, w)
		}
		var ce *CustomRuleError
		if errors.As(cerr, &ce) {
			ce.Index = e.Index
			problems = append(problems, ce)
			continue
		}
		rules = append(rules, r)
	}
	return rules, problems, nil
}

// LoadCustomRules is LoadCustomRuleFile with string warnings, and a missing file is no rules. Invalid entries are
// skipped with a REPORTED warning (index + rule id + reason) — the old silent continue meant a typo'd regex
// quietly removed a rule the operator believed was enforcing. Callers decide where warnings surface.
func LoadCustomRules(path string) ([]Rule, []string, error) {
	rules, problems, err := LoadCustomRuleFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var warnings []string
	for _, p := range problems {
		id := p.RuleID
		if id == "" {
			id = fmt.Sprintf("#%d (no id)", p.Index)
		}
		if p.Warning {
			warnings = append(warnings, fmt.Sprintf("%s: entry %d (rule %q): %v", filepath.Base(path), p.Index, id, p))
			continue
		}
		warnings = append(warnings, fmt.Sprintf("%s: entry %d (rule %q) skipped: %v", filepath.Base(path), p.Index, id, p))
	}
	return rules, warnings, nil
}

// RuleDigest implements analyzer.RuleDigester: it identifies this rule by its
// full CONFIGURATION, not just its ID — two orgs can legitimately define
// different rules under the same ID, and folding only the ID would let org B
// be served org A's cached findings. encoding/json sorts map keys, so the
// digest is stable across processes.
func (r *CustomRule) RuleDigest() string { return r.digest }

// MatchDigest identifies what the rule matches and fixes: its kind, typeMatch, rawTypeMatch, nameMatch,
// propertyHas, propertyNot and autoFix; not its id, name, description, severity, category or suggestion, nor
// schemaVersion (a version step keeps meaning). Baselines persist it as the rule's revision, so a field joins
// customRuleMatch only with omitempty, and existing values never change.
func (r *CustomRule) MatchDigest() string { return r.matchDigest }

// customRuleDigest computes the digest once, at compile time (see the
// digest field). It hashes the config as it marshalled before kind and
// schemaVersion existed: both are cleared at their v1 values (they are
// omitempty) and unknown fields never marshal, so no existing digest moves.
func customRuleDigest(cfg CustomRuleConfig) string {
	if cfg.Kind == CustomRuleKindBlock {
		cfg.Kind = ""
	}
	if cfg.SchemaVersion == 1 {
		cfg.SchemaVersion = 0
	}
	cfg.unknown = nil
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

// customRuleMatch is what MatchDigest hashes.
type customRuleMatch struct {
	Kind         CustomRuleKind    `json:"kind"`
	TypeMatch    string            `json:"typeMatch,omitempty"`
	RawTypeMatch string            `json:"rawTypeMatch,omitempty"`
	NameMatch    string            `json:"nameMatch,omitempty"`
	PropertyHas  map[string]string `json:"propertyHas,omitempty"`
	PropertyNot  map[string]string `json:"propertyNot,omitempty"`
	AutoFix      string            `json:"autoFix,omitempty"`
}

func customRuleMatchDigest(cfg CustomRuleConfig) string {
	m := customRuleMatch{
		Kind:         cfg.Normalized().Kind,
		TypeMatch:    cfg.TypeMatch,
		RawTypeMatch: cfg.RawTypeMatch,
		NameMatch:    cfg.NameMatch,
		PropertyHas:  cfg.PropertyHas,
		PropertyNot:  cfg.PropertyNot,
		AutoFix:      cfg.AutoFix,
	}
	b, err := json.Marshal(m)
	if err != nil {
		b = []byte(fmt.Sprintf("%#v", m))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// NewCustomRule is CompileCustomRule(cfg, CustomRuleTolerant) without the warnings.
func NewCustomRule(cfg CustomRuleConfig) (*CustomRule, error) {
	r, _, err := CompileCustomRule(cfg, CustomRuleTolerant)
	return r, err
}

// CompileCustomRule is the one construction path of a custom rule. It checks, in order:
//  1. kind and schemaVersion: one this build cannot run is an error in every mode;
//  2. unknown fields, a missing id and a built-in rule's id: errors in Strict, warnings in Tolerant;
//  3. severity, nameMatch, rawTypeMatch and autoFix, with the messages they always had.
//
// A rule without a matcher is valid and matches every block; Strict warns about it. The error is the first
// problem, a *CustomRuleError, and the rule is nil with it; warnings come back either way.
func CompileCustomRule(cfg CustomRuleConfig, mode CustomRuleMode) (*CustomRule, []*CustomRuleError, error) {
	r, problems := compileCustomRule(cfg, mode)
	var warnings []*CustomRuleError
	var first *CustomRuleError
	for _, p := range problems {
		if p.Warning {
			warnings = append(warnings, p)
		} else if first == nil {
			first = p
		}
	}
	if first != nil {
		return nil, warnings, first
	}
	return r, warnings, nil
}

// CheckCustomRule lists every problem CompileCustomRule finds under mode, errors and warnings, in its order (for
// lint and per-entry validation).
func CheckCustomRule(cfg CustomRuleConfig, mode CustomRuleMode) []*CustomRuleError {
	_, problems := compileCustomRule(cfg, mode)
	return problems
}

// compileCustomRule runs every check; the rule is nil when any problem is an error.
func compileCustomRule(cfg CustomRuleConfig, mode CustomRuleMode) (*CustomRule, []*CustomRuleError) {
	if p := checkCustomRuleVersion(cfg); p != nil {
		return nil, []*CustomRuleError{p}
	}
	problems := checkCustomRuleShape(cfg, mode)
	invalid := func(field, reason, msg string, err error) {
		problems = append(problems, &CustomRuleError{Index: -1, RuleID: cfg.ID, Field: field, Reason: reason,
			Msg: msg, Err: err})
	}
	r := &CustomRule{
		config: cfg.Normalized(),
		sev:    models.SeverityWarning,
	}
	if cfg.Severity != "" {
		// Validated here because this is where an untrusted string becomes a
		// Severity (from .bakirc/-custom-rules JSON and POST /api/orgs/{id}/rules).
		// An unrecognised value used to be accepted verbatim and then silently
		// dropped by every downstream consumer — the finding vanished from
		// reports, became a PASSING JUnit testcase and could never fail the gate.
		sev := models.Severity(cfg.Severity)
		if sev.Valid() {
			r.sev = sev
		} else {
			invalid("severity", ReasonInvalidSeverity, fmt.Sprintf("custom rule %q: invalid severity %q (must be one of %v)",
				cfg.ID, cfg.Severity, models.ValidSeverities()), nil)
		}
	}
	if cfg.NameMatch != "" {
		re, err := regexp.Compile("(?i)" + cfg.NameMatch)
		if err != nil {
			invalid("nameMatch", ReasonInvalidPattern, err.Error(), err)
		}
		r.nameRe = re
	}
	if cfg.RawTypeMatch != "" {
		re, err := regexp.Compile("(?i)" + cfg.RawTypeMatch)
		if err != nil {
			invalid("rawTypeMatch", ReasonInvalidPattern, err.Error(), err)
		}
		r.rawRe = re
	}
	if cfg.AutoFix != "" && !isValidFixType(cfg.AutoFix) {
		invalid("autoFix", ReasonUnknownAutoFix, fmt.Sprintf("custom rule %q: unknown autoFix %q", cfg.ID, cfg.AutoFix), nil)
	}
	for _, p := range problems {
		if !p.Warning {
			return nil, problems
		}
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
	r.digest = customRuleDigest(cfg)
	r.matchDigest = customRuleMatchDigest(cfg)
	return r, problems
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

// isValidFixType reports whether fixType is a registered fix (LookupFix).
// Used to validate CustomRuleConfig.
func isValidFixType(fixType string) bool {
	_, ok := LookupFix(fixType)
	return ok
}
