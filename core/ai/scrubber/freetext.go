package scrubber

import (
	"regexp"
	"sort"
	"strings"
)

// ScrubFreeText scrubs text that may carry pasted PAD code, without a parse: a chat message, the history,
// the query sent to the embedding provider, an uploaded runbook. The regex pass runs first; it alone misses
// PAD's literal syntax (a SET or a Text: field with a triple-quoted value), so a literal is then masked when
// its line is a SET of a credential-like variable, a `<key>: <literal>` whose key names a credential, a field
// of a sensitiveActions action named on the line, a condition or loop header on a credential-like variable, a
// CASE under such a SWITCH, or when the literal itself passes the entropy check and has PAD's shape — a
// triple-quoted literal anywhere, a '…' or "…" one on a line a PAD rule recognises — since a quoted name in
// prose clears the entropy bar as easily as a token does. A `#` comment line gives up its opaque words, as a
// comment block does in ScrubDocument. The line rules find their keyword anywhere on the line, as a pasted
// line arrives behind a bullet, a quote mark, a line number or a backtick. The rules fire on PAD literals only,
// so prose about credentials ("the password field", "set Password to the vault value", a %Password%
// reference) passes through, and a literal inside a %Var['key']% reference is the key, not a value.
//
// The function is pure and idempotent on its own output, which the stored conversations and the scrub memo
// rely on: the regex pass goes first so both passes judge the same literal text, a masked literal is skipped,
// and the mask carries no anchor. It is linear in the text: the SET and key rules read a short tail before the
// literal, and the per-line facts (keyword, references, the SWITCH above, the actions named) are computed once
// per line.
func ScrubFreeText(text string) string {
	text = ScrubText(text)
	if !strings.ContainsAny(text, `'"#`) {
		return text
	}
	lines := indexLines(text)
	var spans []span
	if strings.ContainsAny(text, `'"`) {
		refs := varRefSpans(text)
		ri, li := 0, 0
		for _, lit := range padLiterals(text) {
			v := text[lit.start:lit.end]
			if v == "" || v == redacted || !isLiteral(v) {
				continue
			}
			for ri < len(refs) && refs[ri].end <= lit.start {
				ri++
			}
			if ri < len(refs) && refs[ri].start <= lit.start && lit.end <= refs[ri].end {
				continue
			}
			for li+1 < len(lines) && lines[li+1].start <= lit.start {
				li++
			}
			if lines[li].comment {
				continue // judged as words below
			}
			holds, padLine := lines.rule(text, li, lit)
			tripleQuoted := lit.start >= 3 && text[lit.start-3:lit.start] == "'''"
			if holds || ((padLine || tripleQuoted) && looksLikeHighEntropySecret(v)) {
				spans = append(spans, lit)
			}
		}
	}
	for i := range lines {
		if lines[i].comment {
			spans = append(spans, opaqueWordSpans(text, lines[i].start+lines[i].hash+1, lines[i].end)...)
		}
	}
	if len(spans) == 0 {
		return text
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	return maskSpans(text, spans)
}

const (
	// ruleTail bounds what the SET and key rules read before a literal: `SET <name> TO ` and `<key>: ` end
	// right at the value, so this many bytes hold them with any name PAD allows.
	ruleTail = 160
)

var (
	// freeTextSet is a SET line up to the literal: `SET <name> TO `, the keyword a word of its own (PAD writes it
	// in capitals; "set" in prose is not it).
	freeTextSet = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])SET\s+(\S+)\s+TO\s*$`)
	// freeTextKey is the `<key>: ` that precedes a property value.
	freeTextKey = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_\-]*)\s*:\s*$`)
	// freeTextHeader is a condition or loop header's keyword, whose literal is judged by the variables it compares.
	freeTextHeader = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])(?:IF|ELSE\s+IF|LOOP\s+WHILE)\s`)
	// freeTextCase is a CASE line, whose literal is judged by the SWITCH header above it; freeTextSwitch is that.
	freeTextCase   = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])CASE\s`)
	freeTextSwitch = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])SWITCH\s`)
	// freeTextAction is an action name on the line (Module.Action), whose fields the `<key>:` pairs after it are.
	freeTextAction = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])([A-Za-z][A-Za-z0-9]*(?:\.[A-Za-z][A-Za-z0-9]*)+)`)
	// varRefSpan is a whole %…% variable reference, property path and indexes included (%Rows[%i%]['Password']%).
	varRefSpan = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*|\[(?:[^\]\n%]|%[^%\]\n]*%)*\])*%`)
)

// lineAction is an action named on a line: where its name ends and the sensitive fields it owns (nil for an
// action the table does not know).
type lineAction struct {
	end    int
	fields []string
}

// textLine is one line of a free text with the facts the rules read, computed once.
type textLine struct {
	start, end int
	// hash is the offset of the '#' of a comment line (the first non-space byte), -1 otherwise.
	hash    int
	comment bool
	// headerAt and caseAt are where a condition/loop header keyword and a CASE keyword start on the line, -1
	// when absent: a literal after the keyword is the header's or the CASE's. Read over the whole line, never a
	// prefix of it, so a mask earlier on the line cannot move a keyword into or out of view between two passes.
	headerAt, caseAt int
	caseOf           int  // for a CASE line: the index of the nearest SWITCH line above, -1 when none
	refs             int8 // refsCredential of the whole line: 0 unknown, 1 no, 2 yes
	// actions are the Module.Action names on the line, ascending, found on the first key rule that needs them.
	actions     []lineAction
	actionsDone bool
}

type textLines []textLine

// indexLines splits text into lines and reads each line once: a comment, the header and CASE keywords, and the
// SWITCH a CASE belongs to.
func indexLines(text string) textLines {
	var lines textLines
	lastSwitch := -1
	for start := 0; start <= len(text); {
		end := len(text)
		if n := strings.IndexByte(text[start:], '\n'); n >= 0 {
			end = start + n
		}
		l := textLine{start: start, end: end, hash: -1, headerAt: -1, caseAt: -1, caseOf: -1}
		line := text[start:end]
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			l.comment, l.hash = true, len(line)-len(trimmed)
		} else {
			if freeTextSwitch.MatchString(line) {
				lastSwitch = len(lines)
			}
			if m := freeTextHeader.FindStringIndex(line); m != nil {
				l.headerAt = start + m[0]
			}
			if m := freeTextCase.FindStringIndex(line); m != nil {
				l.caseAt, l.caseOf = start+m[0], lastSwitch
			}
		}
		lines = append(lines, l)
		start = end + 1
	}
	return lines
}

// refsCredential answers, once per line, whether a %Variable% reference on line i names a credential.
func (ls textLines) refsCredential(text string, i int) bool {
	if ls[i].refs == 0 {
		ls[i].refs = 1
		if refsCredential(text[ls[i].start:ls[i].end]) {
			ls[i].refs = 2
		}
	}
	return ls[i].refs == 2
}

// actionsBefore returns the actions named on line i before offset pos, found once per line.
func (ls textLines) actionsBefore(text string, i, pos int) []lineAction {
	l := &ls[i]
	if !l.actionsDone {
		l.actionsDone = true
		for _, m := range freeTextAction.FindAllStringSubmatchIndex(text[l.start:l.end], -1) {
			l.actions = append(l.actions, lineAction{end: l.start + m[3], fields: sensitiveActions[text[l.start+m[2]:l.start+m[3]]]})
		}
	}
	n := sort.Search(len(l.actions), func(k int) bool { return l.actions[k].end > pos })
	return l.actions[:n]
}

// rule applies the PAD line rules to the literal at lit on line i: the text before the opening delimiter says
// what the literal is the value of. holds is whether that is a credential; padLine whether the line has a PAD
// shape at all (a SET, a header, a CASE, or an action's field), which is what lets the entropy check read a
// single-quoted literal.
func (ls textLines) rule(text string, i int, lit span) (holds, padLine bool) {
	from := max(ls[i].start, lit.start-ruleTail)
	before := strings.TrimSuffix(strings.TrimRight(text[from:lit.start], `'"`), "$")

	if m := freeTextSet.FindStringSubmatch(before); m != nil {
		return isCredentialLikeName(m[1]), true
	}
	if ls[i].headerAt >= 0 && ls[i].headerAt < lit.start {
		return ls.refsCredential(text, i), true
	}
	if ls[i].caseAt >= 0 && ls[i].caseAt < lit.start {
		// The nearest SWITCH above says what the CASE compares.
		if ls[i].caseOf >= 0 {
			return ls.refsCredential(text, ls[i].caseOf), true
		}
		return false, true
	}
	if m := freeTextKey.FindStringSubmatch(before); m != nil {
		if isSensitiveFieldName(m[1]) {
			return true, true
		}
		// An action named earlier on the line (PopulateTextField's Text, FillTextByText's FillWith) says what
		// the field is; a `key:` with no action is prose ("Note: '…'"). A known action wins over any other
		// dotted word on the line (a JWT, a host name), whose masking must not change the verdict.
		actions := ls.actionsBefore(text, i, lit.start)
		if len(actions) == 0 {
			return false, false
		}
		for _, action := range actions {
			for _, f := range action.fields {
				if strings.EqualFold(f, m[1]) {
					return true, true
				}
			}
		}
		return false, true
	}
	return false, false
}

// varRefSpans returns the spans of the %…% variable references in text, ascending.
func varRefSpans(text string) []span {
	var out []span
	for _, m := range varRefSpan.FindAllStringIndex(text, -1) {
		out = append(out, span{m[0], m[1]})
	}
	return out
}

// insideAny reports whether lit lies within one of spans.
func insideAny(spans []span, lit span) bool {
	for _, sp := range spans {
		if sp.start <= lit.start && lit.end <= sp.end {
			return true
		}
	}
	return false
}
