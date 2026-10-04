package parser

import (
	"regexp"
	"strings"
)

var (
	reRegionStart  = regexp.MustCompile(`^#\s*Region\s+"([^"]+)"`)
	reRegionEnd    = regexp.MustCompile(`^#\s*End\s*Region`)
	reSubflowStart = regexp.MustCompile(`^(?i)SUBFLOW\s+(\S+)`)
	reLoopStart    = regexp.MustCompile(`^(?i)(LOOP\s+.+|Loop\.\w+)`)
	reIfStart      = regexp.MustCompile(`^(?i)(IF\s+.+|If\.\w+|Text\.If\b|Variables\.If\b|Display\.If\b)`)
	reElse         = regexp.MustCompile(`^(?i)(ELSE|Else\b|Text\.Else\b|Variables\.Else\b|Display\.Else\b)\s*$`)
	// reElseIf matches the else-if rung of an IF chain. It must be tested before
	// both reIfStart and reElse (neither anchor can match it) — before this
	// pattern existed, `ELSE IF x THEN` matched NOTHING, fell through to UNKNOWN,
	// and every action in the else-if branch silently re-parented into the THEN
	// branch.
	reElseIf       = regexp.MustCompile(`^(?i)ELSE\s+IF\s+.+`)
	reOnErrorStart = regexp.MustCompile(`^(?i)ON\s+(BLOCK\s+)?ERROR\b`)
	// reOnErrorInline matches "ON ERROR REPEAT N TIMES WAIT M" with optional trailing params.
	// Must be checked before reOnErrorStart so extended forms don't create spurious blocks.
	reOnErrorInline = regexp.MustCompile(`^(?i)ON\s+ERROR\s+REPEAT\s+\d+\s+TIMES\s+WAIT`)
	// reOnErrorInlineParams captures the retry count, wait interval, and optional extended params.
	reOnErrorInlineParams = regexp.MustCompile(`(?i)^ON\s+ERROR\s+REPEAT\s+(\d+)\s+TIMES\s+WAIT\s+(\S+)(?:\s+RetryType:\s+(\S+))?(?:\s+MinInterval:\s+(\S+))?(?:\s+MaxInterval:\s+(\S+))?`)
	reComment             = regexp.MustCompile(`^(?i)(COMMENT\s+.*|#\s+.*|//\s+.*)`)
	// Block comments (/# ... #/) are detected with literal HasPrefix/HasSuffix
	// checks in tokenizer.go — the ^/# and #/$ patterns don't need a regex.
	reBlockStart     = regexp.MustCompile(`^(?i)BLOCK(\s+.+)?$`)
	reSwitchStart    = regexp.MustCompile(`^(?i)SWITCH\s+(.*)`)
	reCase           = regexp.MustCompile(`^(?i)CASE\s+(.*)`)
	reDefault        = regexp.MustCompile(`^(?i)DEFAULT\s*$`)
	reDisableCall    = regexp.MustCompile(`^(?i)DISABLE\s+CALL\s+(\S+)`)
	reCall           = regexp.MustCompile(`^(?i)CALL\s+(\S+)`)
	reGoto           = regexp.MustCompile(`^(?i)GOTO\s+'?([^'\s]+)'?`)
	reLabel          = regexp.MustCompile(`^(?i)LABEL\s+'?([^'\s]+)'?`)
	reExit           = regexp.MustCompile(`^(?i)EXIT(\s+FUNCTION|\s+LOOP|\s+Code:.*)?\s*$`)
	reErrorCapture   = regexp.MustCompile(`^(?i)ERROR\s+=>\s*(\w+)`)
	reSetVariable    = regexp.MustCompile(`^(?i)SET\s+(\S+)\s+TO\s+(.*)`)
	reWait           = regexp.MustCompile(`^(?i)WAIT\s+(\d+)`)
	reWaitExpression = regexp.MustCompile(`^(?i)WAIT\s+\(`)
	reDottedAction   = regexp.MustCompile(`^([A-Z][a-zA-Z0-9]*(?:\.[A-Z][a-zA-Z0-9]*)+)\s+(.*)`)
	reOutputVar      = regexp.MustCompile(`=>\s*(\w+)\s*$`)
	// Captures anything between % signs as an expression.
	reVariableRef = regexp.MustCompile(`%([^%]+)%`)
	// Captures PAD string literals: $'''...''', '''...''', '...', or "..."
	reStringLiteral = regexp.MustCompile(`\$?'''[\s\S]*?'''|'[^']*'|"[^"]*"`)
	// Finds potential variable identifiers.
	reIdentifier = regexp.MustCompile(`[A-Za-z_]\w*`)
	// LOOP FOREACH Item IN List — group 1 is the iteration variable, group 3
	// the collection (bare name or %percent-wrapped%).
	reLoopForEach = regexp.MustCompile(`(?i)^LOOP\s+FOREACH\s+(\w+)\s+IN\s+(%?(\w+)%?)`)
	// LOOP LoopIndex FROM x TO y [STEP z] — groups: counter name, FROM, TO,
	// optional STEP. With reLoopForEach and reLoopWhile it gives every loop a
	// KIND, which lets infinite-loop-risk distinguish a loop that CAN run
	// forever from one bounded by construction.
	reLoopWhile = regexp.MustCompile(`(?i)^LOOP\s+WHILE\b`)
	// reLoopRangeAnon is the counted form WITHOUT a named counter (`LOOP FROM
	// 2 TO %FreeRow% - 1`): exists only to recognise the shape, bounded either way.
	reLoopRangeAnon = regexp.MustCompile(`(?i)^LOOP\s+FROM\s+`)
	reLoopRange     = regexp.MustCompile(`(?i)^LOOP\s+(\w+)\s+FROM\s+(-?%?[\w.]+%?)\s+TO\s+(-?%?[\w.]+%?)(?:\s+STEP\s+(-?%?[\w.]+%?))?`)
	// **REGION / **ENDREGION — alternative inline region syntax used in some PAD exports.
	reStarRegionStart = regexp.MustCompile(`(?i)^\*\*REGION\s*(.*)`)
	reStarRegionEnd   = regexp.MustCompile(`(?i)^\*\*ENDREGION`)
	// NEXT LOOP — PAD's continue statement (skip to next iteration).
	reNextLoop = regexp.MustCompile(`(?i)^NEXT\s+LOOP$`)
	// EXIT LOOP — PAD's break statement (exit loop early).
	reExitLoop = regexp.MustCompile(`(?i)^EXIT\s+LOOP$`)
)

// isExpressionKeyword reports AND/OR/NOT/TRUE/FALSE/NULL/BLANK,
// case-insensitively — a 7-case switch, an order of magnitude cheaper than
// the regex engine call it replaced (invoked per identifier inside %expr%).
func isExpressionKeyword(vname string) bool {
	// EqualFold per case: ToUpper allocated a fresh string for every
	// mixed-case identifier inside every %expr%.
	switch {
	case strings.EqualFold(vname, "AND"),
		strings.EqualFold(vname, "OR"),
		strings.EqualFold(vname, "NOT"),
		strings.EqualFold(vname, "TRUE"),
		strings.EqualFold(vname, "FALSE"),
		strings.EqualFold(vname, "NULL"),
		strings.EqualFold(vname, "BLANK"):
		return true
	}
	return false
}

// hasPrefixFold reports whether s starts with prefix, case-insensitively,
// without allocating (unlike ToUpper+HasPrefix).
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// maskStrings blanks out string-literal content so downstream %var% regexes
// only match outside literals. Byte-wise on purpose: the delimiters are ASCII
// and UTF-8 continuation bytes can never collide with them, so byte
// comparisons are exact and allocation-free. The masked text is only used for
// regex MATCHING (positions never map back), so one space per literal is fine.
func maskStrings(s string) string {
	// No literal can start without a quote byte: a %expr% without one
	// returns unchanged — maskStrings runs twice per block and most
	// expressions hold no literals.
	if !strings.ContainsAny(s, "'\"") {
		return s
	}
	out := make([]byte, 0, len(s))
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		// $'''...''' (interpolated) or '''...''' literal.
		if c == '$' && i+3 < n && s[i+1] == '\'' && s[i+2] == '\'' && s[i+3] == '\'' {
			end := indexTripleQuote(s, i+4)
			out = append(out, ' ')
			i = end
			continue
		}
		if c == '\'' && i+2 < n && s[i+1] == '\'' && s[i+2] == '\'' {
			end := indexTripleQuote(s, i+3)
			out = append(out, ' ')
			i = end
			continue
		}
		// '...' or "..." literal.
		if c == '\'' || c == '"' {
			j := i + 1
			for j < n && s[j] != c {
				j++
			}
			out = append(out, ' ')
			i = j + 1 // past the closing quote (or n if unterminated)
			continue
		}
		out = append(out, c)
		i++
	}
	return string(out)
}

// indexTripleQuote returns the index just past the next ”' at or after
// from, or len(s) when the literal is unterminated.
func indexTripleQuote(s string, from int) int {
	i := from
	n := len(s)
	for i+2 < n {
		if s[i] == '\'' && s[i+1] == '\'' && s[i+2] == '\'' {
			return i + 3
		}
		i++
	}
	return n
}
