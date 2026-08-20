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
	reOnErrorStart = regexp.MustCompile(`^(?i)ON\s+(BLOCK\s+)?ERROR\b`)
	// reOnErrorInline matches "ON ERROR REPEAT N TIMES WAIT M" with optional trailing params
	// (e.g. RetryType: RetryType.Exponential MinInterval: 2 MaxInterval: 360).
	// Must be checked before reOnErrorStart so extended forms don't create spurious blocks.
	reOnErrorInline = regexp.MustCompile(`^(?i)ON\s+ERROR\s+REPEAT\s+\d+\s+TIMES\s+WAIT`)
	// reOnErrorInlineParams captures the retry count, wait interval, and optional extended params.
	reOnErrorInlineParams = regexp.MustCompile(`(?i)^ON\s+ERROR\s+REPEAT\s+(\d+)\s+TIMES\s+WAIT\s+(\S+)(?:\s+RetryType:\s+(\S+))?(?:\s+MinInterval:\s+(\S+))?(?:\s+MaxInterval:\s+(\S+))?`)
	reComment             = regexp.MustCompile(`^(?i)(COMMENT\s+.*|#\s+.*|//\s+.*)`)
	reBlockComment        = regexp.MustCompile(`^/#`)
	reBlockCommentEnd     = regexp.MustCompile(`#/$`)
	reBlockStart          = regexp.MustCompile(`^(?i)BLOCK(\s+.+)?$`)
	reSwitchStart         = regexp.MustCompile(`^(?i)SWITCH\s+(.*)`)
	reCase                = regexp.MustCompile(`^(?i)CASE\s+(.*)`)
	reDefault             = regexp.MustCompile(`^(?i)DEFAULT\s*$`)
	reDisableCall         = regexp.MustCompile(`^(?i)DISABLE\s+CALL\s+(\S+)`)
	reCall                = regexp.MustCompile(`^(?i)CALL\s+(\S+)`)
	reGoto                = regexp.MustCompile(`^(?i)GOTO\s+'?([^'\s]+)'?`)
	reLabel               = regexp.MustCompile(`^(?i)LABEL\s+'?([^'\s]+)'?`)
	reExit                = regexp.MustCompile(`^(?i)EXIT(\s+FUNCTION|\s+LOOP|\s+Code:.*)?\s*$`)
	reErrorCapture        = regexp.MustCompile(`^(?i)ERROR\s+=>\s*(\w+)`)
	reSetVariable         = regexp.MustCompile(`^(?i)SET\s+(\S+)\s+TO\s+(.*)`)
	reWait                = regexp.MustCompile(`^(?i)WAIT\s+(\d+)`)
	reWaitExpression      = regexp.MustCompile(`^(?i)WAIT\s+\(`)
	reDottedAction        = regexp.MustCompile(`^([A-Z][a-zA-Z0-9]*(?:\.[A-Z][a-zA-Z0-9]*)+)\s+(.*)`)
	reOutputVar           = regexp.MustCompile(`=>\s*(\w+)\s*$`)
	// Captures anything between % signs as an expression.
	reVariableRef = regexp.MustCompile(`%([^%]+)%`)
	// Captures PAD string literals: $'''...''', '''...''', '...', or "..."
	reStringLiteral = regexp.MustCompile(`\$?'''[\s\S]*?'''|'[^']*'|"[^"]*"`)
	// Finds potential variable identifiers.
	reIdentifier = regexp.MustCompile(`[A-Za-z_]\w*`)
	// Keywords to ignore when extracting variables from expressions —
	// matched by isExpressionKeyword (a switch, kept in sync with this list
	// for documentation).
	// LOOP FOREACH Item IN List — captures the iteration variable name (group 1)
	// and the collection reference (group 2 = full form, group 3 = bare name).
	// The collection may appear as a bare name ("List") or percent-wrapped ("%List%").
	reLoopForEach = regexp.MustCompile(`(?i)^LOOP\s+FOREACH\s+(\w+)\s+IN\s+(%?(\w+)%?)`)
	// LOOP LoopIndex FROM x TO y [STEP z] — captures:
	//   group 1: counter variable name (e.g. "LoopIndex")
	//   group 2: FROM value (e.g. "0" or "%StartVal%")
	//   group 3: TO value   (e.g. "10" or "%EndVal%")
	//   group 4: STEP value (optional, e.g. "2" or "%Step%")
	// The value pattern (-?%?[\w.]+%?) covers plain numbers, negative numbers,
	// and percent-wrapped variable references.
	reLoopRange = regexp.MustCompile(`(?i)^LOOP\s+(\w+)\s+FROM\s+(-?%?[\w.]+%?)\s+TO\s+(-?%?[\w.]+%?)(?:\s+STEP\s+(-?%?[\w.]+%?))?`)
	// **REGION / **ENDREGION — alternative inline region syntax used in some PAD exports.
	reStarRegionStart = regexp.MustCompile(`(?i)^\*\*REGION\s*(.*)`)
	reStarRegionEnd   = regexp.MustCompile(`(?i)^\*\*ENDREGION`)
	// NEXT LOOP — PAD's continue statement (skip to next iteration).
	reNextLoop = regexp.MustCompile(`(?i)^NEXT\s+LOOP$`)
	// EXIT LOOP — PAD's break statement (exit loop early).
	reExitLoop = regexp.MustCompile(`(?i)^EXIT\s+LOOP$`)
)

// isExpressionKeyword reports whether an identifier is one of the expression
// keywords (AND/OR/NOT/TRUE/FALSE/NULL/BLANK), case-insensitively — a
// 7-case switch, an order of magnitude cheaper than the regex engine call it
// replaced (invoked per identifier inside every %expr%).
func isExpressionKeyword(vname string) bool {
	switch strings.ToUpper(vname) {
	case "AND", "OR", "NOT", "TRUE", "FALSE", "NULL", "BLANK":
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
// only match outside literals. Byte-wise on purpose: the delimiters (' " $)
// are ASCII, and UTF-8 continuation bytes (≥0x80) can never collide with
// them, so byte comparisons are exact and allocation-free — the previous
// rune-slice version allocated a fresh string per rune-window comparison.
// The masked text is only used for regex MATCHING (positions never map back
// to the original), so replacing each literal with a single space is fine.
func maskStrings(s string) string {
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

// indexTripleQuote returns the index just past the next ''' at or after
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
