package parser

import "regexp"

var (
	reRegionStart    = regexp.MustCompile(`^#\s*Region\s+"([^"]+)"`)
	reRegionEnd      = regexp.MustCompile(`^#\s*End\s*Region`)
	reSubflowStart   = regexp.MustCompile(`^(?i)SUBFLOW\s+(\S+)`)
	reLoopStart      = regexp.MustCompile(`^(?i)(LOOP\s+.+|Loop\.\w+)`)
	reIfStart        = regexp.MustCompile(`^(?i)(IF\s+.+|If\.\w+|Text\.If\b|Variables\.If\b|Display\.If\b)`)
	reElse           = regexp.MustCompile(`^(?i)(ELSE|Else\b|Text\.Else\b|Variables\.Else\b|Display\.Else\b)\s*$`)
	reOnErrorStart   = regexp.MustCompile(`^(?i)ON\s+(BLOCK\s+)?ERROR\b`)
	reOnErrorInline  = regexp.MustCompile(`^(?i)ON\s+ERROR\s+REPEAT\s+\d+\s+TIMES\s+WAIT\s+\d+\s*$`)
	reComment        = regexp.MustCompile(`^(?i)(COMMENT\s+.*|#\s+.*|//\s+.*)`)
	reBlockComment   = regexp.MustCompile(`^/#`)
	reBlockCommentEnd = regexp.MustCompile(`#/$`)
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
	rePropertyPair   = regexp.MustCompile(`(\w[\w.]*):\s*`)
	// Captures anything between % signs as an expression.
	reVariableRef    = regexp.MustCompile(`%([^%]+)%`)
	// Captures PAD string literals: $'''...''', '''...''', '...', or "..."
	reStringLiteral  = regexp.MustCompile(`\$?'''[\s\S]*?'''|'[^']*'|"[^"]*"`)
	// Finds potential variable identifiers.
	reIdentifier     = regexp.MustCompile(`[A-Za-z_]\w*`)
	// Keywords to ignore when extracting variables from expressions.
	reExpressionKeyword = regexp.MustCompile(`(?i)^(AND|OR|NOT|TRUE|FALSE|NULL|BLANK)$`)
	// LOOP FOREACH Item IN %List% — captures the iteration variable name.
	reLoopForEach    = regexp.MustCompile(`(?i)^LOOP\s+FOREACH\s+(\w+)\s+IN\s+`)
	// LOOP FROM x TO y STEP z — range loops create an implicit CurrentItem counter.
	reLoopRange      = regexp.MustCompile(`(?i)^LOOP\s+FROM\s+`)
)

func maskStrings(s string) string {
	runes := []rune(s)
	n := len(runes)
	for i := 0; i < n; i++ {
		// Handle $'''...''' and '''...'''
		if (i+3 < n && string(runes[i:i+4]) == "$'''") || (i+2 < n && string(runes[i:i+3]) == "'''") {
			start := i
			isInterpolated := runes[i] == '$'
			if isInterpolated {
				i += 4
			} else {
				i += 3
			}
			
			// Find end '''
			for i+2 < n {
				if string(runes[i:i+3]) == "'''" {
					i += 2
					break
				}
				runes[i] = ' '
				i++
			}
			// Mask the markers too if we want full isolation, but here we just mask content
			for j := start; j <= i && j < n; j++ {
				runes[j] = ' '
			}
			continue
		}

		// Handle '...' and "..."
		r := runes[i]
		if r == '\'' || r == '"' {
			start := i
			quoteChar := r
			i++
			for i < n && runes[i] != quoteChar {
				runes[i] = ' '
				i++
			}
			// Mask quotes
			for j := start; j <= i && j < n; j++ {
				runes[j] = ' '
			}
			continue
		}
	}
	return string(runes)
}
