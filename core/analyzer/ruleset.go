package analyzer

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	"pad-core/models"
)

// ruleSetDigestPrefix versions the RuleSet digest. Digests are persisted
// (baselines, health snapshots, notice IDs), so a new encoding needs a new
// prefix, and a recorded set with another prefix reads as legacy.
const ruleSetDigestPrefix = "rs1:"

// matchDigester is implemented by configuration-defined rules (CustomRule):
// their revision is the digest of what they match and fix.
type matchDigester interface {
	MatchDigest() string
}

// RuleRefOf returns r's reference as it runs now: a built-in's revision
// ("" at Revision <= 1, "r<N>" above) and stage from its RuleMeta, a custom
// rule's MatchDigest and its own (stable) stage.
func RuleRefOf(r Rule) models.RuleRef {
	return ruleRef(metaTable(), r)
}

func ruleRef(t ruleMetaTable, r Rule) models.RuleRef {
	ref := models.RuleRef{ID: r.ID()}
	var stage Stage
	if d, ok := r.(matchDigester); ok {
		ref.Rev = d.MatchDigest()
		stage = MetaOf(r).Stage
	} else if m := t[ref.ID]; m != nil {
		if m.Revision > 1 {
			ref.Rev = "r" + strconv.Itoa(m.Revision)
		}
		stage = m.Stage
	}
	if stage == StagePreview {
		ref.Stage = string(StagePreview)
	}
	return ref
}

// RecordRuleSet records the rules an analysis ran; the engine stamps it on
// AnalysisReport.RuleSet before the report is cached.
func RecordRuleSet(rules []Rule) *models.RuleSet {
	t := metaTable()
	refs := make([]models.RuleRef, len(rules))
	for i, r := range rules {
		refs[i] = ruleRef(t, r)
	}
	return ruleSetOf(refs)
}

// NewRuleSet builds the RuleSet of refs (the input is not modified): sorted
// by (ID, Rev, Stage), duplicates dropped, and digested as
// "rs1:" + hex SHA-256 over every ref's ID, Rev and Stage, each written as
// its decimal byte length, ":" and the bytes. Order and duplicates do not
// change the digest; the encoding is pinned by TestRuleSetDigest_Pinned.
func NewRuleSet(refs []models.RuleRef) *models.RuleSet {
	return ruleSetOf(slices.Clone(refs))
}

func ruleSetOf(refs []models.RuleRef) *models.RuleSet {
	slices.SortFunc(refs, compareRuleRefs)
	refs = slices.Compact(refs)
	if refs == nil {
		refs = []models.RuleRef{}
	}
	h := sha256.New()
	var buf []byte
	for _, r := range refs {
		buf = buf[:0]
		for _, field := range [...]string{r.ID, r.Rev, r.Stage} {
			buf = strconv.AppendInt(buf, int64(len(field)), 10)
			buf = append(buf, ':')
			buf = append(buf, field...)
		}
		h.Write(buf)
	}
	return &models.RuleSet{Rules: refs, Digest: ruleSetDigestPrefix + hex.EncodeToString(h.Sum(nil))}
}

func compareRuleRefs(a, b models.RuleRef) int {
	if c := cmp.Compare(a.ID, b.ID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Rev, b.Rev); c != 0 {
		return c
	}
	return cmp.Compare(a.Stage, b.Stage)
}

// KnownToLegacyRecords reports whether a record without a usable rule set
// (written before rule sets, rewritten by a release that did not know them,
// or whose rules do not match its digest) saw the rule ref: every custom rule
// (a MatchDigest Rev), every rule that is not a built-in, and the built-ins
// of rule-set version 1 at their first revision (ADR 0016).
func KnownToLegacyRecords(ref models.RuleRef) bool {
	if !builtinRev(ref.Rev) {
		return true
	}
	m := lookupMetaRef(ref.ID)
	return m == nil || (m.IntroducedIn <= 1 && m.Revision <= 1)
}

// builtinRev reports whether rev is a built-in's revision ("" or "r<N>"); a
// custom rule's MatchDigest is lower-case hex and never starts with 'r'.
func builtinRev(rev string) bool {
	if rev == "" {
		return true
	}
	n, ok := strings.CutPrefix(rev, "r")
	if !ok || n == "" {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// RuleAge tells whether a finding's rule is newer than a recorded rule set:
// absent from it, revised, or promoted (its current ref is not in the record).
// The zero value treats every rule as known.
type RuleAge struct {
	newRule map[string]bool // rule ID -> every current ref with this ID is new
}

// NewRuleAge compares the current rule set (the report's) with a recorded
// one. A recorded digest equal to the current one knows every rule, even
// without its Rules (a digest-only read). Otherwise a nil recorded set, or one
// whose Rules do not hash to its Digest (missing, partial, or a newer encoding
// read after a rollback), is legacy (KnownToLegacyRecords). A nil current set
// knows nothing new: every finding stays drift, the status quo.
func NewRuleAge(recorded, current *models.RuleSet) RuleAge {
	if current == nil || (recorded != nil && recorded.Digest == current.Digest) {
		return RuleAge{}
	}
	var seen map[models.RuleRef]bool
	legacy := recorded == nil || NewRuleSet(recorded.Rules).Digest != recorded.Digest
	if !legacy {
		seen = make(map[models.RuleRef]bool, len(recorded.Rules))
		for _, r := range recorded.Rules {
			seen[r] = true
		}
	}
	a := RuleAge{newRule: make(map[string]bool, len(current.Rules))}
	for _, ref := range current.Rules {
		var isNew bool
		if legacy {
			isNew = !KnownToLegacyRecords(ref)
		} else {
			isNew = !seen[ref]
		}
		// Two refs under one ID (a custom rule reusing a built-in's ID):
		// new only if both are, so the finding fails toward drift.
		if prev, ok := a.newRule[ref.ID]; ok {
			isNew = prev && isNew
		}
		a.newRule[ref.ID] = isNew
	}
	return a
}

// IsNew reports whether f's rule is newer than the recorded set. A rule the
// current set does not name (unregistered, the parse-error fallback of a rule
// list without it) is not new: its findings stay drift.
func (a RuleAge) IsNew(f *models.Finding) bool {
	return a.newRule[f.RuleID]
}

// DriftSplit partitions findings (a baseline's new findings, or a report's
// findings for a like-for-like health check) by what they may raise.
type DriftSplit struct {
	Drift      []models.Finding // gating, from a rule the record knew: drift alerts
	NewRule    []models.Finding // gating, from a rule newer than the record: one notice, no drift
	Preview    []models.Finding // from a preview rule: never alerts, never gates
	NewRuleIDs []string         // NewRule's rule IDs, sorted, distinct
}

// SplitDrift partitions newFindings (BaselineDrift.New) by Gates and age.
func SplitDrift(newFindings []models.Finding, age RuleAge) DriftSplit {
	var s DriftSplit
	t := metaTable()
	for i := range newFindings {
		f := &newFindings[i]
		switch {
		case !gates(t, f):
			s.Preview = append(s.Preview, *f)
		case age.IsNew(f):
			s.NewRule = append(s.NewRule, *f)
			if !slices.Contains(s.NewRuleIDs, f.RuleID) {
				s.NewRuleIDs = append(s.NewRuleIDs, f.RuleID)
			}
		default:
			s.Drift = append(s.Drift, *f)
		}
	}
	slices.Sort(s.NewRuleIDs)
	return s
}

// Annotate fills d's split fields (NewRuleIDs, NewFromNewRules, Preview); d.New
// and its counts keep every new finding.
func (s DriftSplit) Annotate(d *models.BaselineDrift) {
	d.NewRuleIDs = s.NewRuleIDs
	d.NewFromNewRules = len(s.NewRule)
	d.Preview = len(s.Preview)
}

// HealthOf is the health score of findings under m's complexity terms
// (MaxCyclomatic, MaxCognitive, CircularDependencies; none when m is nil),
// with the formula behind FlowMetrics.HealthScore: HealthOf(r.Findings,
// r.Metrics) == r.Metrics.HealthScore, and a subset (SplitDrift's Drift over
// a report's findings) is a like-for-like score. Preview findings never count.
func HealthOf(findings []models.Finding, m *models.FlowMetrics) int {
	if m == nil {
		return healthScore(findings, 0, 0, false)
	}
	return healthScore(findings, m.MaxCyclomatic, m.MaxCognitive, len(m.CircularDependencies) > 0)
}
