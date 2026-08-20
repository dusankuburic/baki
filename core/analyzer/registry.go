package analyzer

// registeredRules accumulates every Rule via each rule file's init() (see
// registerRule) — so a new rule_*.go file is picked up by AllRules() the
// moment it adds its own `func init() { registerRule(&XRule{}) }`, with no
// separate "also add it to registry.go" step to forget. This replaced a
// hand-maintained literal slice that had already drifted out of sync with the
// rule files at least once.
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
// an AutoFix (see mayEmitAutoFix). The apply-fix loop only ever selects
// findings with a non-empty AutoFix, so running the loop's analysis with just
// this subset is behavior-equivalent while dispatching ~60% fewer rules per
// iteration. Order is preserved from the input so fixable findings are picked
// in the same sequence as with the full set.
func AutoFixableRules(rules []Rule) []Rule {
	out := make([]Rule, 0, len(rules)/2)
	for _, r := range rules {
		if mayEmitAutoFix(r) {
			out = append(out, r)
		}
	}
	return out
}
