package analyzer

// registeredRules accumulates every Rule via each rule file's init() (see
// registerRule) — a new rule_*.go file is picked up by AllRules() automatically
// (this replaced a hand-maintained literal slice that had already drifted out
// of sync at least once).
var registeredRules []Rule

// registerRule adds r to the rule catalog. Called from each rule file's
// init(); not intended to be called directly elsewhere.
func registerRule(r Rule) {
	registeredRules = append(registeredRules, r)
}

// AllRules returns every registered rule. Returns a fresh copy so a caller
// that filters/reorders its result can't mutate the shared registry.
func AllRules() []Rule {
	out := make([]Rule, len(registeredRules))
	copy(out, registeredRules)
	return out
}

// AutoFixableRules returns the subset of rules that can emit findings carrying
// an AutoFix (see mayEmitAutoFix). The apply-fix loop only selects findings
// with a non-empty AutoFix, so running it with just this subset is
// behavior-equivalent with ~60% fewer rules per iteration; order is preserved
// so fixable findings are picked in the same sequence.
func AutoFixableRules(rules []Rule) []Rule {
	out := make([]Rule, 0, len(rules)/2)
	for _, r := range rules {
		if mayEmitAutoFix(r) {
			out = append(out, r)
		}
	}
	return out
}
