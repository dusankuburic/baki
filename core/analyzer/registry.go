package analyzer

// registeredRules accumulates every Rule via each rule file's init() (see
// registerRule) — a new rule_*.go file is picked up by AllRules() automatically
// (this replaced a hand-maintained literal slice that had already drifted out
// of sync at least once).
var registeredRules []Rule

// The rules whose findings are per subflow or per document are flow rules;
// custom rules match blocks. TestAllRules_HaveAKind covers every registered rule.
var (
	_ FlowRule  = (*CircularSubflowRule)(nil)
	_ FlowRule  = (*DuplicateSubflowNameRule)(nil)
	_ FlowRule  = (*EmptySubflowRule)(nil)
	_ FlowRule  = (*HighCyclomaticComplexityRule)(nil)
	_ FlowRule  = (*LargeSubflowRule)(nil)
	_ FlowRule  = (*ParseErrorRule)(nil)
	_ FlowRule  = (*SubflowNoErrorHandlerRule)(nil)
	_ FlowRule  = (*UncalledSubflowRule)(nil)
	_ BlockRule = (*CustomRule)(nil)
)

// registerRule adds r and its metadata to the rule catalog. Called from each
// rule file's init() (single-threaded); not intended to be called elsewhere.
// Completeness of m is enforced by meta_test.go, not at init.
func registerRule(r Rule, m RuleMeta) {
	registeredRules = append(registeredRules, r)
	t := metaTable()
	if t == nil {
		t = ruleMetaTable{}
		ruleMetas.Store(&t)
	}
	t[r.ID()] = &m
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

// mayEmitAutoFix mirrors stampFindings, which stamps by RuleID: the built-in
// metadata under r's ID names a fix (stamped or direct), or r is a custom rule
// with an autoFix.
func mayEmitAutoFix(r Rule) bool {
	if m := lookupMetaRef(r.ID()); m != nil && (m.AutoFix != "" || len(m.DirectFixes) > 0) {
		return true
	}
	if cr, ok := r.(*CustomRule); ok {
		return cr.config.AutoFix != ""
	}
	return false
}
