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
// sibling block. A directive among the LEADING comments of a subflow (or of the
// file) additionally covers that subflow's (or the document's) findings that
// have no block to anchor on — see inlineSuppressions.
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

// inlineSuppressions is the parsed `pad-ignore` state of one flow, in three
// scopes:
//
//   - blocks: the next-block scope — a directive applies to the first concrete
//     sibling after it (the model documented above). Keyed by target block ID.
//   - subflows: the subflow-header scope — directives among the LEADING comment
//     lines of a subflow (before its first real block) also apply to that
//     subflow's block-less findings. Keyed by subflow ID.
//   - file: the file scope — the header of the first subflow that has any blocks
//     (the top of Main.txt, or of the implicit "Main" region) also applies to
//     block-less findings anywhere in the document.
//
// The last two exist because some findings have no block to anchor on. The
// engine's parse-error fallback emits findings with an empty BlockID when no
// block of the first subflow is parseable, and the per-block parse-error rule
// anchors on Blocks[0] of the first subflow — which, when the author writes a
// directive there, IS the directive comment, a block the next-block scope can
// never name. Either way `# pad-ignore[parse-error]` at the top of the file
// silenced nothing. Header directives therefore also cover findings anchored
// on the header comments themselves.
//
// The rule sets use the same sentinel as before: suppressAll means every rule.
type inlineSuppressions struct {
	blocks   map[string]map[string]bool
	subflows map[string]map[string]bool
	file     map[string]bool
}

func (s *inlineSuppressions) empty() bool {
	return s == nil || (len(s.blocks) == 0 && len(s.subflows) == 0 && len(s.file) == 0)
}

// suppresses reports whether finding f is silenced by any scope.
func (s *inlineSuppressions) suppresses(f *models.Finding) bool {
	if s == nil {
		return false
	}
	if ruleSetHas(s.blocks[f.BlockID], f.RuleID) {
		return true
	}
	if f.BlockID != "" {
		return false
	}
	return ruleSetHas(s.subflows[f.SubflowID], f.RuleID) || ruleSetHas(s.file, f.RuleID)
}

func ruleSetHas(set map[string]bool, rule string) bool {
	return set != nil && (set[suppressAll] || set[rule])
}

// mergeDirective folds one parsed directive into set.
func mergeDirective(set map[string]bool, all bool, rules []string) {
	if all {
		set[suppressAll] = true
	}
	for _, r := range rules {
		set[r] = true
	}
}

func mergeRuleSet(dst map[string]map[string]bool, key string, src map[string]bool) {
	m := dst[key]
	if m == nil {
		m = map[string]bool{}
		dst[key] = m
	}
	for k := range src {
		m[k] = true
	}
}

// leadingCommentIDs returns the IDs of the comment blocks that open a sibling
// group before its first non-comment block — the subflow header when called on
// a subflow's top-level blocks.
func leadingCommentIDs(blocks []models.Block) []string {
	var ids []string
	for i := range blocks {
		if blocks[i].Type != models.BlockTypeComment {
			break
		}
		ids = append(ids, blocks[i].ID)
	}
	return ids
}

// collectInlineSuppressions walks the flow and returns every `pad-ignore`
// directive resolved into its scopes (see inlineSuppressions). Each sibling
// group (and nested child group) is its own next-block scope, so a directive
// only reaches the next concrete block in the same group; only a subflow's
// top-level leading comments form a header.
func collectInlineSuppressions(flow *models.FlowDocument) *inlineSuppressions {
	supp := &inlineSuppressions{
		blocks:   map[string]map[string]bool{},
		subflows: map[string]map[string]bool{},
	}

	// walk processes one sibling group. header is non-nil only for a subflow's
	// top-level group and receives the directives found among its leading
	// comments.
	var walk func(blocks []models.Block, header map[string]bool)
	walk = func(blocks []models.Block, header map[string]bool) {
		pending := map[string]bool{}
		inHeader := header != nil
		for i := range blocks {
			b := &blocks[i]
			if b.Type == models.BlockTypeComment {
				if all, rules, ok := parsePadIgnore(b.Name); ok {
					mergeDirective(pending, all, rules)
					if inHeader {
						mergeDirective(header, all, rules)
					}
				}
				continue // comments never carry findings; keep accumulating
			}
			inHeader = false

			if len(pending) > 0 {
				mergeRuleSet(supp.blocks, b.ID, pending)
				pending = map[string]bool{}
			}

			if len(b.Children) > 0 {
				walk(b.Children, nil)
			}
		}
		// Findings anchored on the header comments themselves (the parse-error
		// rule anchors on Blocks[0]) are covered by the header directives.
		if len(header) > 0 {
			for _, id := range leadingCommentIDs(blocks) {
				mergeRuleSet(supp.blocks, id, header)
			}
		}
	}

	fileScoped := false
	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		header := map[string]bool{}
		walk(sf.Blocks, header)
		if len(header) > 0 {
			supp.subflows[sf.ID] = header
		}
		if !fileScoped && len(sf.Blocks) > 0 {
			fileScoped = true
			if len(header) > 0 {
				supp.file = header
			}
		}
	}
	return supp
}

// applyInlineSuppressions removes findings silenced by a `pad-ignore` directive
// in any scope, returning the kept findings and the number suppressed (counted
// per finding, so stats.Suppressed stays a finding count).
func applyInlineSuppressions(findings []models.Finding, supp *inlineSuppressions) ([]models.Finding, int) {
	if supp.empty() {
		return findings, 0
	}
	kept := make([]models.Finding, 0, len(findings))
	suppressed := 0
	for i := range findings {
		if supp.suppresses(&findings[i]) {
			suppressed++
			continue
		}
		kept = append(kept, findings[i])
	}
	return kept, suppressed
}
