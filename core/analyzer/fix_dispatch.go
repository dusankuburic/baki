package analyzer

import (
	"fmt"
	"sort"

	"pad-core/models"
)

// FixFlag describes a registered fix.
type FixFlag uint8

const (
	// FixBehaviourChanging marks a fix that alters what the flow DOES at
	// runtime, as opposed to how it handles failure. Nothing used to enforce
	// that: the fix loop applied any finding with a non-empty AutoFix, so
	// `bakicli fix` applied them unattended and the round-trip gate reported
	// success because the finding had disappeared — never asking whether the
	// flow still means the same thing. Such fixes need the explicit opt-in
	// (FixOptions.IncludeBehaviourChanging, the API's unsafe flag).
	//
	// Deliberately NOT set on remove-block (deleting an unreachable block does
	// not change a successful run — the risk is the ANALYSIS being wrong) nor
	// on parameterize-sql (behaviour-changing only if PAD lacks bind-parameter
	// support at that call site, which could not be verified — gating it on a
	// guess would be its own kind of wrong).
	FixBehaviourChanging FixFlag = 1 << iota
	// FixUsesVariable: the builder reads the finding's Metadata["variable"].
	FixUsesVariable
	// FixUsesProperty: the builder reads the finding's Metadata["property"].
	FixUsesProperty
	// FixUsesRuleID: the builder reads the finding's rule ID.
	FixUsesRuleID
	// FixStandalone: offered for any finding rather than through a rule's
	// metadata (suppress), so no rule needs to reference it.
	FixStandalone
)

// FixInput is what a fix builder gets: the target block plus the finding's
// rule ID and subject (Metadata "variable" / "property").
type FixInput struct {
	Block                      *models.Block
	RuleID, Variable, Property string
}

// FixBuilder builds a fix's patch; an empty patch means "nothing to change".
type FixBuilder func(FixInput) models.Patch

// FixSpec is one registered fix type. Fix types are stored data (findings,
// org custom rules, conversation fix proposals): never rename one.
type FixSpec struct {
	Type  string
	Build FixBuilder
	Flags FixFlag
}

var fixRegistry = map[string]FixSpec{}

// registerFix adds a fix type; autofix.go calls it next to each builder as a
// package variable initialiser, so the registry is complete before any init().
func registerFix(fixType string, build FixBuilder, flags FixFlag) struct{} {
	if _, dup := fixRegistry[fixType]; dup {
		panic("analyzer: fix type registered twice: " + fixType)
	}
	fixRegistry[fixType] = FixSpec{Type: fixType, Build: build, Flags: flags}
	return struct{}{}
}

// LookupFix returns the registered fix type t.
func LookupFix(t string) (FixSpec, bool) {
	s, ok := fixRegistry[t]
	return s, ok
}

// FixTypes returns every registered fix, sorted by Type.
func FixTypes() []FixSpec {
	out := make([]FixSpec, 0, len(fixRegistry))
	for _, s := range fixRegistry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// PatchForFix builds the patch for a deterministic auto-fix through the fix
// registry: the single source of truth shared by FlowService.generateFixPatch
// and the headless CLI. variable/property come from the finding's Metadata;
// ruleID is used only by the "suppress" fix. Unknown fixType ⇒ empty patch + error.
func PatchForFix(block *models.Block, fixType, ruleID, variable, property string) (models.Patch, error) {
	s, ok := fixRegistry[fixType]
	if !ok {
		return models.Patch{}, fmt.Errorf("unknown fix type %q", fixType)
	}
	return s.Build(FixInput{Block: block, RuleID: ruleID, Variable: variable, Property: property}), nil
}

// IsBehaviourChangingFix reports whether fixType alters runtime behaviour and so
// requires an explicit opt-in before an unattended loop applies it.
func IsBehaviourChangingFix(fixType string) bool {
	return fixRegistry[fixType].Flags&FixBehaviourChanging != 0
}
