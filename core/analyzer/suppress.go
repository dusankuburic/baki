package analyzer

import (
	"regexp"
	"strings"

	"pad-core/models"
)

// Inline suppression: a PAD comment immediately before an action silences that
// next block (the ESLint eslint-disable-next-line model), everywhere the
// analyzer runs — web app, CLI, batch, governance scanner. Case-insensitive:
// `# pad-ignore` (all rules) or `# pad-ignore[rule1, rule2]`
// (`pad-ignore-next-line[...]` alias; consecutive directives stack); among a
// subflow's leading comments it also covers that subflow's block-less findings.
// The blanket form must be the ENTIRE comment (the old pattern matched the word
// anywhere and "never use pad-ignore in production flows" failed the CI gate
// open); a malformed list matches NEITHER pattern, so a typo'd directive
// suppresses nothing and the finding stays visible.
var (
	rePadIgnoreRules = regexp.MustCompile(`(?i)\bpad-ignore(?:-next-line)?\[([^\]]*)\]`)
	rePadIgnoreAll   = regexp.MustCompile(`^(?i)pad-ignore(?:-next-line)?$`)
)

// suppressAll is the sentinel rule key meaning "every rule is suppressed on this
// block". It cannot collide with a real rule ID (those are kebab-case words).
const suppressAll = "*"

// parsePadIgnore reports whether text contains a pad-ignore directive and, if
// so, whether it suppresses all rules (all=true) or a specific set (rules); an
// empty or whitespace-only bracket list means all.
func parsePadIgnore(text string) (all bool, rules []string, ok bool) {
	// Block comments keep their /# framing in Name; strip it so `/# pad-ignore #/` works too.
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "/#")
	t = strings.TrimSuffix(t, "#/")
	t = strings.TrimSpace(t)

	m := rePadIgnoreRules.FindStringSubmatch(t)
	if m == nil {
		// No well-formed list: only a comment that IS the bare directive
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
		// Lower-cased: (?i) only covered the keyword, so `pad-ignore[UNHANDLED-ERROR]`
		// matched no rule ID and silently suppressed nothing.
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			rules = append(rules, p)
		}
	}
	if len(rules) == 0 {
		return true, nil, true
	}
	return false, rules, true
}

// inlineSuppressions is the parsed `pad-ignore` state in three scopes: blocks
// (next-block, keyed by target block ID), subflows (a subflow's leading
// comments, keyed by subflow ID) and file (first subflow header). The last two
// cover block-less findings: parse-error findings carry an empty BlockID, and
// the per-block parse-error rule anchors on Blocks[0] of the first subflow,
// which when the author writes a directive there IS the directive comment —
// a block next-block scope can never name.
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
// group before its first non-comment block (a subflow header at top level).
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

// collectInlineSuppressions resolves every `pad-ignore` directive into its
// scopes: each sibling (and nested) group is its own next-block scope, so a
// directive only reaches the next concrete block in the same group; only a
// subflow's top-level leading comments form a header.
func collectInlineSuppressions(flow *models.FlowDocument) *inlineSuppressions {
	supp := &inlineSuppressions{
		blocks:   map[string]map[string]bool{},
		subflows: map[string]map[string]bool{},
	}

	// walk processes one sibling group; header is non-nil only for a subflow's
	// top-level group and receives its leading directives.
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
// in any scope, returning the kept findings and the count suppressed (per
// finding, so stats.Suppressed stays a finding count).
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
