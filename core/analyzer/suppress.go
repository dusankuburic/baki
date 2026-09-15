package analyzer

import (
	"regexp"
	"strings"

	"pad-core/models"
)

// Inline suppression lets flow authors silence findings directly in the flow
// source using a PAD comment placed immediately before the action it applies to
// (the same model as ESLint's `// eslint-disable-next-line`). Because it lives in
// the flow text, it is honored everywhere the analyzer runs — the web app, the
// CLI, batch analysis and the governance scanner — so a CI gate can respect a
// reviewed false-positive without a database round-trip.
//
// Syntax (case-insensitive), written as a normal PAD comment line:
//
//	# pad-ignore                      -> suppress every rule on the next block
//	# pad-ignore[hardcoded-credential] -> suppress one rule on the next block
//	# pad-ignore[deep-nesting, unused-variable] -> suppress several rules
//	COMMENT  pad-ignore-next-line[sql-injection-risk]  (alias form also works)
//
// Consecutive comment directives stack and all apply to the next non-comment
// sibling block.
// Two patterns, because the two forms carry very different risk.
//
// rePadIgnoreRules (targeted) may appear anywhere in the comment: naming
// explicit rule IDs is self-evidently deliberate, and writing the justification
// first — "reviewed, false positive pad-ignore[sql-injection-risk]" — is a
// documented and tested usage.
//
// rePadIgnoreAll (blanket) must be the ENTIRE comment. This is the fail-open
// that mattered: the old single pattern was unanchored with an OPTIONAL bracket
// group, so any mention of the word anywhere matched with an empty capture and
// fell through to "suppress every rule". A comment reading
//
//	COMMENT TODO: never use pad-ignore in production flows
//
// disabled all 45 rules on the next action, Security rules included — and
// bakicli is the product's CI gate, so it failed open exactly where it must
// not. Requiring the blanket form to stand alone removes the ambiguity without
// touching the targeted form.
//
// A malformed list now matches NEITHER pattern rather than degrading to
// suppress-all: `pad-ignore[unhandled-error` (missing `]`) suppresses nothing.
// That is the safe direction and it is self-correcting — the finding the author
// meant to silence still appears, so the typo is visible.
var (
	rePadIgnoreRules = regexp.MustCompile(`(?i)\bpad-ignore(?:-next-line)?\[([^\]]*)\]`)
	rePadIgnoreAll   = regexp.MustCompile(`^(?i)pad-ignore(?:-next-line)?$`)
)

// suppressAll is the sentinel rule key meaning "every rule is suppressed on this
// block". It cannot collide with a real rule ID (those are kebab-case words).
const suppressAll = "*"

// parsePadIgnore reports whether text contains a pad-ignore directive and, if so,
// whether it suppresses all rules (all=true) or a specific set (rules). A
// directive with an empty or whitespace-only bracket list suppresses all rules.
func parsePadIgnore(text string) (all bool, rules []string, ok bool) {
	// Block comments keep their /# framing in Name; strip it so `/# pad-ignore #/`
	// is honoured the same as the single-line forms, whose marker the tokenizer
	// has already removed.
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "/#")
	t = strings.TrimSuffix(t, "#/")
	t = strings.TrimSpace(t)

	m := rePadIgnoreRules.FindStringSubmatch(t)
	if m == nil {
		// No well-formed rule list: only a comment that IS the bare directive
		// suppresses anything, and it suppresses everything.
		if rePadIgnoreAll.MatchString(t) {
			return true, nil, true
		}
		return false, nil, false
	}
	inside := strings.TrimSpace(m[1])
	if inside == "" {
		// `pad-ignore[]` — explicit and unambiguous, so it still means "all".
		return true, nil, true
	}
	for _, part := range strings.Split(inside, ",") {
		// Lower-cased because rule IDs are kebab-case and this directive is
		// documented as case-insensitive. The (?i) flag only ever covered the
		// keyword, so `pad-ignore[UNHANDLED-ERROR]` captured the uppercase text
		// verbatim, matched no rule ID, and silently suppressed nothing.
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			rules = append(rules, p)
		}
	}
	if len(rules) == 0 {
		return true, nil, true
	}
	return false, rules, true
}

// collectInlineSuppressions walks the flow and returns, per target block ID, the
// set of rule IDs suppressed by preceding `pad-ignore` comment siblings. The
// special key suppressAll means all rules are suppressed for that block. Each
// sibling group (and nested child group) is its own scope, so a directive only
// reaches the next concrete block in the same group.
func collectInlineSuppressions(flow *models.FlowDocument) map[string]map[string]bool {
	supp := map[string]map[string]bool{}

	var walk func(blocks []models.Block)
	walk = func(blocks []models.Block) {
		pending := map[string]bool{}
		for i := range blocks {
			b := &blocks[i]
			if b.Type == models.BlockTypeComment {
				if all, rules, ok := parsePadIgnore(b.Name); ok {
					if all {
						pending[suppressAll] = true
					}
					for _, r := range rules {
						pending[r] = true
					}
				}
				continue // comments never carry findings; keep accumulating
			}

			if len(pending) > 0 {
				m := supp[b.ID]
				if m == nil {
					m = map[string]bool{}
					supp[b.ID] = m
				}
				for k := range pending {
					m[k] = true
				}
				pending = map[string]bool{}
			}

			if len(b.Children) > 0 {
				walk(b.Children)
			}
		}
	}

	for i := range flow.Subflows {
		walk(flow.Subflows[i].Blocks)
	}
	return supp
}

// applyInlineSuppressions removes findings whose target block has a matching
// `pad-ignore` directive, returning the kept findings and the number suppressed.
func applyInlineSuppressions(findings []models.Finding, supp map[string]map[string]bool) ([]models.Finding, int) {
	if len(supp) == 0 {
		return findings, 0
	}
	kept := make([]models.Finding, 0, len(findings))
	suppressed := 0
	for _, f := range findings {
		if m := supp[f.BlockID]; m != nil && (m[suppressAll] || m[f.RuleID]) {
			suppressed++
			continue
		}
		kept = append(kept, f)
	}
	return kept, suppressed
}
