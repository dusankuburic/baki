package analyzer

import (
	"slices"
	"sort"
	"sync/atomic"

	"pad-core/models"
)

// Stage is a rule's release stage. A preview rule runs by default, but its
// findings are stamped Finding.Stage = "preview" and never gate (ADR 0014).
type Stage string

const (
	StageStable  Stage = "stable"
	StagePreview Stage = "preview"
)

// RuleSetVersion identifies the built-in rule set. A release that adds a rule
// bumps it by one and gives the new rule IntroducedIn = the new value; every
// rule shipped before Phase 4a is version 1.
const RuleSetVersion = 1

// OptionSpec declares an integer threshold a rule reads from its settings
// Options (see ruleOption): the default, and the minimum below which a
// configured value is ignored.
type OptionSpec struct {
	Key          string
	Default, Min int
}

// Related names a rule whose findings are worth reviewing together with this
// rule's (editorial; GET /api/analysis/dependencies).
type Related struct {
	To, Reason string
}

// RuleMeta is what the catalog knows about a rule beyond its Rule methods.
// Built-ins declare it in their file's init (registerRule); custom rules
// derive it from their config (CustomRule.Meta).
//
// Confidence High means deterministic with a low false-positive rate, Low a
// heuristic or style check with frequent false positives.
type RuleMeta struct {
	Confidence        models.Confidence // stamped on findings that carry none
	AutoFix           string            // fix type stamped on findings that carry none
	DirectFixes       []string          // fix types Check stamps itself (subflow-mismatch: append-output)
	DisabledByDefault bool              // runs only when settings enable it
	Options           []OptionSpec
	Related           []Related
	IntroducedIn      int // RuleSetVersion that shipped the rule
	Revision          int // bumped when existing findings can change; 0 or 1 = first revision
	Stage             Stage
}

func (m RuleMeta) clone() RuleMeta {
	m.DirectFixes = slices.Clone(m.DirectFixes)
	m.Options = slices.Clone(m.Options)
	m.Related = slices.Clone(m.Related)
	return m
}

// ruleMetaTable maps a built-in rule ID to its metadata. registerRule fills it
// during init; SetRuleMetaForTesting swaps in a copy, so readers load it
// atomically and never see a map being written.
type ruleMetaTable map[string]*RuleMeta

var ruleMetas atomic.Pointer[ruleMetaTable]

func metaTable() ruleMetaTable {
	if p := ruleMetas.Load(); p != nil {
		return *p
	}
	return nil
}

func lookupMetaRef(id string) *RuleMeta { return metaTable()[id] }

// LookupMeta returns the metadata of the built-in rule id.
func LookupMeta(id string) (RuleMeta, bool) {
	if m := lookupMetaRef(id); m != nil {
		return m.clone(), true
	}
	return RuleMeta{}, false
}

// MetaOf returns r's metadata: a custom rule's own, else the built-in
// registered under r.ID() (test wrappers embed a real rule and keep its ID),
// else Medium confidence and stable.
func MetaOf(r Rule) RuleMeta {
	if cr, ok := r.(*CustomRule); ok {
		return cr.Meta()
	}
	if m, ok := LookupMeta(r.ID()); ok {
		return m
	}
	return RuleMeta{Confidence: models.ConfidenceMedium, Stage: StageStable}
}

// Meta is a custom rule's metadata: Medium confidence, stable, its autoFix as
// a direct fix (Check stamps it). Custom rules carry no rule-set version.
func (r *CustomRule) Meta() RuleMeta {
	m := RuleMeta{Confidence: models.ConfidenceMedium, Stage: StageStable}
	if r.config.AutoFix != "" {
		m.DirectFixes = []string{r.config.AutoFix}
	}
	return m
}

// IsPreview reports whether the built-in rule id is in the preview stage.
func IsPreview(id string) bool {
	m := lookupMetaRef(id)
	return m != nil && m.Stage == StagePreview
}

// RuleEnabled reports whether r runs under s: the settings entry when there is
// one, else !MetaOf(r).DisabledByDefault. Persisted rule maps are never
// rewritten, so an absent entry must mean the rule's own default.
func RuleEnabled(r Rule, s *models.AppSettings) bool {
	if s != nil {
		if rc, ok := s.Analysis.Rules[r.ID()]; ok {
			return rc.Enabled
		}
	}
	if _, ok := r.(*CustomRule); ok {
		return true // custom rules are never disabled by default
	}
	m := lookupMetaRef(r.ID())
	return m == nil || !m.DisabledByDefault
}

// RuleConfidence returns a rule's built-in default confidence, or Medium when unknown.
func RuleConfidence(id string) models.Confidence {
	if m := lookupMetaRef(id); m != nil && m.Confidence != "" {
		return m.Confidence
	}
	return models.ConfidenceMedium
}

// RuleAutoFix returns the fix type the engine stamps on the rule's findings, or "".
func RuleAutoFix(id string) string {
	if m := lookupMetaRef(id); m != nil {
		return m.AutoFix
	}
	return ""
}

// DefaultRuleConfigs derives the built-in rule configuration from the
// registry: enabled unless DisabledByDefault, DefaultSeverity, option
// defaults. models.DefaultSettings keeps a literal copy (core/models cannot
// import the analyzer); TestDefaultSettings_MirrorsRuleMeta keeps them equal.
func DefaultRuleConfigs() map[string]models.RuleConfig {
	t := metaTable()
	out := make(map[string]models.RuleConfig, len(registeredRules))
	for _, r := range registeredRules {
		rc := models.RuleConfig{Enabled: true, Severity: string(r.DefaultSeverity())}
		if m := t[r.ID()]; m != nil {
			rc.Enabled = !m.DisabledByDefault
			if len(m.Options) > 0 {
				rc.Options = make(map[string]interface{}, len(m.Options))
				for _, o := range m.Options {
					rc.Options[o.Key] = o.Default
				}
			}
		}
		out[r.ID()] = rc
	}
	return out
}

// RelatedRules flattens every built-in's Related list, sorted by (from, to).
func RelatedRules() []models.RuleDependency {
	t := metaTable()
	var out []models.RuleDependency
	for _, r := range registeredRules {
		m := t[r.ID()]
		if m == nil {
			continue
		}
		for _, rel := range m.Related {
			out = append(out, models.RuleDependency{FromRuleID: r.ID(), ToRuleID: rel.To, Reason: rel.Reason})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromRuleID != out[j].FromRuleID {
			return out[i].FromRuleID < out[j].FromRuleID
		}
		return out[i].ToRuleID < out[j].ToRuleID
	})
	return out
}

// SetRuleMetaForTesting replaces (or adds, for a test fake's ID) the metadata
// under id until the returned func runs. Test binaries only, and not from
// parallel tests: the swap is process-wide. Cached reports need no reset:
// ProfileDigest folds the metadata the engine stamps, so a flip misses.
func SetRuleMetaForTesting(id string, m RuleMeta) (restore func()) {
	prev := ruleMetas.Load()
	next := make(ruleMetaTable, len(metaTable())+1)
	for k, v := range metaTable() {
		next[k] = v
	}
	m = m.clone()
	next[id] = &m
	ruleMetas.Store(&next)
	return func() { ruleMetas.Store(prev) }
}
