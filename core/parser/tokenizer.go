package parser

import (
	"bufio"
	"strings"
)

func computeIndent(line string) int {
	n := 0
	for _, ch := range line {
		switch ch {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			// Stop at the first non-whitespace rune: indentation is leading
			// whitespace only. (Must return, not break — a switch `break` would
			// only exit the switch and keep counting interior whitespace.)
			return n
		}
	}
	return n
}

func Tokenize(text string) []Token {
	if text == "" {
		return nil
	}

	tokens := make([]Token, 0, 512)
	scanner := bufio.NewScanner(strings.NewReader(text))
	// Support lines up to 1MB
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	inBlockComment := false
	commentStartLine := 0
	commentIndent := 0
	var commentRaw strings.Builder

	inTripleQuote := false
	tripleStartLine := 0
	var tripleRaw strings.Builder

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := scanner.Text()

		if lineNum == 1 && strings.HasPrefix(raw, "\xEF\xBB\xBF") {
			raw = raw[3:]
		}

		trimmed := strings.TrimRight(raw, " \t\r")
		indent := computeIndent(trimmed)
		content := strings.TrimLeft(trimmed, " \t")

		if inBlockComment {
			commentRaw.WriteString(raw)
			commentRaw.WriteByte('\n')
			if reBlockCommentEnd.MatchString(content) {
				inBlockComment = false
				fullText := commentRaw.String()
				tokens = append(tokens, Token{
					Kind:    TokComment,
					Line:    commentStartLine,
					Indent:  commentIndent,
					Raw:     fullText,
					Content: fullText,
					Name:    strings.TrimSpace(fullText),
					RawType: "COMMENT",
				})
			}
			continue
		}

		if inTripleQuote {
			tripleRaw.WriteByte('\n')
			tripleRaw.WriteString(raw)
			if countTripleQuotes(raw)%2 == 1 {
				inTripleQuote = !inTripleQuote
			}
			if !inTripleQuote {
				combined := tripleRaw.String()
				trimmed2 := strings.TrimRight(combined, " \t\r")
				indent2 := computeIndent(trimmed2)
				content2 := strings.TrimLeft(trimmed2, " \t")
				tok := classifyLine(tripleStartLine, indent2, combined, content2)
				// The token spans from tripleStartLine through this closing
				// line; record the physical end so block fixers append/wrap/
				// remove past the literal rather than inside it.
				tok.EndLine = lineNum
				tokens = append(tokens, tok)
			}
			continue
		}

		if content == "" {
			tokens = append(tokens, Token{
				Kind:    TokEmpty,
				Line:    lineNum,
				Indent:  indent,
				Raw:     raw,
				Content: "",
			})
			continue
		}

		// Single-line block comment: /#...#/ entirely on one line.
		if reBlockComment.MatchString(content) && reBlockCommentEnd.MatchString(content) {
			tokens = append(tokens, Token{
				Kind:    TokComment,
				Line:    lineNum,
				Indent:  indent,
				Raw:     raw,
				Content: content,
				Name:    strings.TrimSpace(content),
				RawType: "COMMENT",
			})
			continue
		}

		if reBlockComment.MatchString(content) && !reBlockCommentEnd.MatchString(content) {
			inBlockComment = true
			commentStartLine = lineNum
			commentIndent = indent
			commentRaw.Reset()
			commentRaw.WriteString(raw)
			commentRaw.WriteByte('\n')
			continue
		}

		tok := classifyLine(lineNum, indent, raw, content)

		// Triple-quoted string literals ($'''...''') only appear inside ACTION
		// lines. Running this check on every line type silently swallowed the
		// rest of the file when a comment/region/GOTO line happened to contain
		// an odd number of ''' — the just-classified token was discarded and
		// every subsequent line was absorbed until another odd-''' line closed
		// the span. Restrict the multi-line mode to ACTION tokens.
		if tok.Kind == TokAction && countTripleQuotes(raw)%2 == 1 {
			inTripleQuote = true
			tripleStartLine = lineNum
			tripleRaw.Reset()
			tripleRaw.WriteString(raw)
			continue
		}

		tokens = append(tokens, tok)
	}

	if err := scanner.Err(); err != nil {
		tokens = append(tokens, Token{
			Kind:    TokError,
			Line:    lineNum + 1,
			Raw:     err.Error(),
			Content: err.Error(),
			Name:    err.Error(),
		})
	}

	return tokens
}

func countTripleQuotes(line string) int {
	count := 0
	for i := 0; i <= len(line)-3; {
		if line[i] == '\'' && line[i+1] == '\'' && line[i+2] == '\'' {
			count++
			i += 3
		} else {
			i++
		}
	}
	return count
}

// classifyLine dispatches a single logical PAD line to a Token by trying each
// syntax category in turn — the FIRST category whose pattern matches wins, so
// this order must not change. Each classify* helper is an independent,
// self-contained "does this line look like X?" check (no shared mutable state
// between them), which is what makes this decomposition safe: it is pure code
// movement, not a behavior change. A line matching nothing falls through to
// the generic dotted-action classifier, then finally UNKNOWN.
func classifyLine(lineNum, indent int, raw, content string) Token {
	base := tokenBase{lineNum: lineNum, indent: indent, raw: raw, content: content}

	classifiers := []func(tokenBase) (Token, bool){
		classifyComment,
		classifyRegion,
		classifyBlockControl,
		classifyErrorHandler,
		classifyLoopStart,
		classifyCondition,
		classifyCallOrLabel,
		classifyLoopControl,
		classifyStatement,
		classifyGenericAction,
	}
	for _, classify := range classifiers {
		if tok, ok := classify(base); ok {
			return tok
		}
	}

	return Token{
		Kind:    TokError,
		Line:    lineNum,
		Indent:  indent,
		Raw:     raw,
		Content: content,
		Name:    content,
		RawType: "UNKNOWN",
	}
}

// tokenBase holds the fields every classify* helper needs, so each one reads
// as "given this line, is it an X?" without repeating the same four-argument
// list classifyLine itself takes.
type tokenBase struct {
	lineNum, indent int
	raw, content    string
}

// classifyComment matches `#`/`//`/`COMMENT` line comments and a same-line
// block-comment close (`#/` alone, i.e. the tail of a multi-line block comment
// Tokenize already consumed).
func classifyComment(b tokenBase) (Token, bool) {
	content := b.content
	// Region directives (#Region/#EndRegion AND the spaced "# Region"/
	// "# EndRegion" variants) are structural, not comments — defer to
	// classifyRegion. Without this, reComment's "#\s+.*" alternative steals the
	// spaced forms (which it matches) and the subflow boundary is misclassified
	// as a comment, corrupting the block tree with spurious unclosed-block errors.
	if reRegionStart.MatchString(content) || reRegionEnd.MatchString(content) {
		return Token{}, false
	}
	if reComment.MatchString(content) {
		name := content
		// Allocation-free case-insensitive prefix checks (ToUpper allocated
		// a fresh string per line even for non-matching lines).
		if hasPrefixFold(content, "COMMENT") {
			name = strings.TrimSpace(content[len("COMMENT"):])
		} else if strings.HasPrefix(content, "#") && !reRegionStart.MatchString(content) {
			name = strings.TrimSpace(content[1:])
		} else if strings.HasPrefix(content, "//") {
			name = strings.TrimSpace(content[2:])
		}
		return Token{
			Kind:    TokComment,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    name,
			RawType: "COMMENT",
		}, true
	}

	if reBlockCommentEnd.MatchString(content) && strings.HasPrefix(strings.TrimSpace(content), "#/") {
		return Token{
			Kind:    TokComment,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    strings.TrimSpace(content),
			RawType: "COMMENT",
		}, true
	}

	return Token{}, false
}

// classifyRegion matches the two region-boundary syntaxes (`**REGION`/
// `**ENDREGION` and `#region`/`#endregion`) plus `SUBFLOW:` — all of which
// delimit a named block container or subflow boundary.
func classifyRegion(b tokenBase) (Token, bool) {
	content := b.content

	// **REGION name / **ENDREGION — alternative inline region syntax (double-star form).
	// Treated as named block containers rather than subflow boundaries.
	if m := reStarRegionStart.FindStringSubmatch(content); m != nil {
		name := strings.TrimSpace(m[1])
		if name == "" {
			name = "Region"
		}
		return Token{
			Kind:    TokBlockStart,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    name,
			RawType: "REGION",
		}, true
	}

	if reStarRegionEnd.MatchString(content) {
		return Token{
			Kind:    TokEnd,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "",
			RawType: "ENDREGION",
		}, true
	}

	if m := reRegionStart.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokSubflowStart,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    m[1],
			RawType: "Region",
		}, true
	}

	if reRegionEnd.MatchString(content) {
		return Token{
			Kind:    TokSubflowEnd,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "",
			RawType: "EndRegion",
		}, true
	}

	if m := reSubflowStart.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokSubflowStart,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    m[1],
			RawType: "SUBFLOW",
		}, true
	}

	return Token{}, false
}

// classifyBlockControl matches END/END SUBFLOW, an action-block header
// (`BLOCK 'name'`), SWITCH, CASE, and DEFAULT.
func classifyBlockControl(b tokenBase) (Token, bool) {
	content := b.content
	trimmed := strings.TrimSpace(content)

	if strings.EqualFold(trimmed, "END") || strings.EqualFold(trimmed, "END SUBFLOW") {
		return Token{
			Kind:    TokEnd,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "End",
			RawType: "END",
		}, true
	}

	if m := reBlockStart.FindStringSubmatch(content); m != nil {
		name := ""
		if len(m) > 1 && m[1] != "" {
			name = strings.Trim(strings.TrimSpace(m[1]), "'")
		}
		return Token{
			Kind:    TokBlockStart,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    name,
			RawType: "BLOCK",
		}, true
	}

	if m := reSwitchStart.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokSwitchStart,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    content,
			RawType: "SWITCH",
		}, true
	}

	if reCase.MatchString(content) {
		return Token{
			Kind:    TokCase,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    content,
			RawType: "CASE",
		}, true
	}

	if reDefault.MatchString(content) {
		return Token{
			Kind:    TokDefault,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "Default",
			RawType: "DEFAULT",
		}, true
	}

	return Token{}, false
}

// classifyErrorHandler matches an inline `ON ERROR` annotation (kept as a
// TokComment — it decorates the preceding action rather than starting a new
// block) and an `ON BLOCK ERROR` handler-block header.
func classifyErrorHandler(b tokenBase) (Token, bool) {
	content := b.content

	if reOnErrorInline.MatchString(content) {
		return Token{
			Kind:    TokComment,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    content,
			RawType: "ON_ERROR_INLINE",
		}, true
	}

	if reOnErrorStart.MatchString(content) {
		return Token{
			Kind:    TokErrorHandlerStart,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "On Block Error",
			RawType: "OnBlockError",
		}, true
	}

	return Token{}, false
}

// classifyLoopStart matches a loop-block header (FOR/LOOP/FOR EACH/…),
// preferring the specific dotted-action name (e.g. "LOOP.LoopForEach") as
// RawType when present so downstream rules can distinguish loop kinds.
func classifyLoopStart(b tokenBase) (Token, bool) {
	content := b.content
	if !reLoopStart.MatchString(content) {
		return Token{}, false
	}
	rawType := "LOOP"
	name := content
	if m := reDottedAction.FindStringSubmatch(content); m != nil {
		rawType = m[1]
	}
	return Token{
		Kind:    TokLoopStart,
		Line:    b.lineNum,
		Indent:  b.indent,
		Raw:     b.raw,
		Content: content,
		Name:    name,
		RawType: rawType,
	}, true
}

// classifyCondition matches an IF-block header (stripping a trailing THEN
// keyword from the display name) and ELSE.
func classifyCondition(b tokenBase) (Token, bool) {
	content := b.content

	if reIfStart.MatchString(content) {
		rawType := "IF"
		name := content
		if m := reDottedAction.FindStringSubmatch(content); m != nil {
			rawType = m[1]
		}
		// Strip trailing THEN keyword: "IF %x% > 0 THEN" → "IF %x% > 0"
		// (case-insensitive suffix check, allocation-free).
		trimmed := strings.TrimSpace(name)
		if len(trimmed) >= 5 && strings.EqualFold(trimmed[len(trimmed)-5:], " THEN") {
			name = strings.TrimSpace(name[:len(name)-5])
		}
		return Token{
			Kind:    TokConditionStart,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    name,
			RawType: rawType,
		}, true
	}

	if reElse.MatchString(content) {
		return Token{
			Kind:    TokConditionElse,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "Else",
			RawType: "ELSE",
		}, true
	}

	return Token{}, false
}

// classifyCallOrLabel matches subflow calls (including disabled calls), GOTO,
// and LABEL statements.
func classifyCallOrLabel(b tokenBase) (Token, bool) {
	content := b.content

	if m := reDisableCall.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "Call " + m[1] + " (disabled)",
			RawType: "DISABLED_CALL",
		}, true
	}

	if m := reCall.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "Call " + m[1],
			RawType: "CALL",
		}, true
	}

	if m := reGoto.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    m[1],
			RawType: "GOTO",
		}, true
	}

	if m := reLabel.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    m[1],
			RawType: "LABEL",
		}, true
	}

	return Token{}, false
}

// classifyLoopControl matches EXIT LOOP, CONTINUE LOOP (next), and the
// standalone EXIT statement.
func classifyLoopControl(b tokenBase) (Token, bool) {
	content := b.content

	if reExitLoop.MatchString(content) {
		return Token{
			Kind:    TokAction,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "Exit Loop",
			RawType: "EXIT_LOOP",
		}, true
	}

	if reNextLoop.MatchString(content) {
		return Token{
			Kind:    TokAction,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "Next Loop",
			RawType: "NEXT_LOOP",
		}, true
	}

	if reExit.MatchString(content) {
		return Token{
			Kind:    TokAction,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    content,
			RawType: "EXIT",
		}, true
	}

	return Token{}, false
}

// classifyStatement matches ON ERROR's error-capture variable assignment,
// SET, and WAIT statements.
func classifyStatement(b tokenBase) (Token, bool) {
	content := b.content

	if m := reErrorCapture.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    "Error => " + m[1],
			RawType: "ERROR_CAPTURE",
		}, true
	}

	if m := reSetVariable.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:        TokAction,
			Line:        b.lineNum,
			Indent:      b.indent,
			Raw:         b.raw,
			Content:     content,
			Name:        "Set " + m[1],
			RawType:     "SET",
			SetVarName:  m[1],
			SetVarValue: strings.TrimSpace(m[2]),
		}, true
	}

	if reWait.MatchString(content) || reWaitExpression.MatchString(content) {
		name := content
		if m := reDottedAction.FindStringSubmatch(content); m != nil {
			name = "Wait " + m[1]
		}
		return Token{
			Kind:    TokAction,
			Line:    b.lineNum,
			Indent:  b.indent,
			Raw:     b.raw,
			Content: content,
			Name:    name,
			RawType: "WAIT",
		}, true
	}

	return Token{}, false
}

// classifyGenericAction is the catch-all for any `Module.Action(params)`
// action line that didn't match a more specific category above.
func classifyGenericAction(b tokenBase) (Token, bool) {
	content := b.content
	m := reDottedAction.FindStringSubmatch(content)
	if m == nil {
		return Token{}, false
	}
	return Token{
		Kind:    TokAction,
		Line:    b.lineNum,
		Indent:  b.indent,
		Raw:     b.raw,
		Content: content,
		Name:    extractActionName(m[1], m[2]),
		RawType: m[1],
	}, true
}

func extractActionName(moduleAction, params string) string {
	parts := strings.Split(moduleAction, ".")
	if len(parts) >= 2 {
		return splitCamelCase(parts[len(parts)-1])
	}
	return moduleAction
}

func splitCamelCase(s string) string {
	var result []rune
	runes := []rune(s)
	n := len(runes)
	for i, r := range runes {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := runes[i-1]
			// Transition: lowercase→Uppercase (getText → Get Text)
			if prev >= 'a' && prev <= 'z' {
				result = append(result, ' ')
			} else if prev >= 'A' && prev <= 'Z' && i+1 < n && runes[i+1] >= 'a' && runes[i+1] <= 'z' {
				// Transition: end of acronym (HTTPServer → HTTP Server)
				result = append(result, ' ')
			}
		}
		result = append(result, r)
	}
	return string(result)
}
