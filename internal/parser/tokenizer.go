package parser

import (
	"bufio"
	"strings"
)

func computeIndent(line string) int {
	n := 0
	for _, ch := range line {
		if ch == ' ' {
			n++
		} else if ch == '\t' {
			n += 4
		} else {
			break
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

		if countTripleQuotes(raw)%2 == 1 {
			inTripleQuote = true
			tripleStartLine = lineNum
			tripleRaw.Reset()
			tripleRaw.WriteString(raw)
			continue
		}

		tokens = append(tokens, tok)
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

func classifyLine(lineNum, indent int, raw, content string) Token {
	upper := strings.ToUpper(strings.TrimSpace(content))

	if reComment.MatchString(content) {
		name := content
		if strings.HasPrefix(upper, "COMMENT") {
			name = strings.TrimSpace(content[len("COMMENT"):])
		} else if strings.HasPrefix(content, "#") && !reRegionStart.MatchString(content) {
			name = strings.TrimSpace(content[1:])
		} else if strings.HasPrefix(content, "//") {
			name = strings.TrimSpace(content[2:])
		}
		return Token{
			Kind:    TokComment,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    name,
			RawType: "COMMENT",
		}
	}

	if reBlockCommentEnd.MatchString(content) && strings.HasPrefix(strings.TrimSpace(content), "#/") {
		return Token{
			Kind:    TokComment,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    strings.TrimSpace(content),
			RawType: "COMMENT",
		}
	}

	if m := reRegionStart.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokSubflowStart,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    m[1],
			RawType: "Region",
		}
	}

	if reRegionEnd.MatchString(content) {
		return Token{
			Kind:    TokSubflowEnd,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    "",
			RawType: "EndRegion",
		}
	}

	if m := reSubflowStart.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokSubflowStart,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    m[1],
			RawType: "SUBFLOW",
		}
	}

	if upper == "END" || upper == "END SUBFLOW" {
		return Token{
			Kind:    TokEnd,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    "End",
			RawType: "END",
		}
	}

	if m := reBlockStart.FindStringSubmatch(content); m != nil {
		name := ""
		if len(m) > 1 && m[1] != "" {
			name = strings.Trim(strings.TrimSpace(m[1]), "'")
		}
		return Token{
			Kind:    TokBlockStart,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    name,
			RawType: "BLOCK",
		}
	}

	if m := reSwitchStart.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokSwitchStart,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    content,
			RawType: "SWITCH",
		}
	}

	if reCase.MatchString(content) {
		return Token{
			Kind:    TokCase,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    content,
			RawType: "CASE",
		}
	}

	if reDefault.MatchString(content) {
		return Token{
			Kind:    TokDefault,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    "Default",
			RawType: "DEFAULT",
		}
	}

	if reOnErrorInline.MatchString(content) {
		return Token{
			Kind:    TokComment,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    content,
			RawType: "ON_ERROR_INLINE",
		}
	}

	if reOnErrorStart.MatchString(content) {
		return Token{
			Kind:    TokErrorHandlerStart,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    "On Block Error",
			RawType: "OnBlockError",
		}
	}

	if reLoopStart.MatchString(content) {
		rawType := "LOOP"
		name := content
		if m := reDottedAction.FindStringSubmatch(content); m != nil {
			rawType = m[1]
		}
		return Token{
			Kind:    TokLoopStart,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    name,
			RawType: rawType,
		}
	}

	if reIfStart.MatchString(content) {
		rawType := "IF"
		name := content
		if m := reDottedAction.FindStringSubmatch(content); m != nil {
			rawType = m[1]
		}
		// Strip trailing THEN keyword: "IF %x% > 0 THEN" → "IF %x% > 0"
		if upper := strings.ToUpper(strings.TrimSpace(name)); strings.HasSuffix(upper, " THEN") {
			name = strings.TrimSpace(name[:len(name)-5])
		}
		return Token{
			Kind:    TokConditionStart,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    name,
			RawType: rawType,
		}
	}

	if reElse.MatchString(content) {
		return Token{
			Kind:    TokConditionElse,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    "Else",
			RawType: "ELSE",
		}
	}

	if m := reDisableCall.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    "Call " + m[1] + " (disabled)",
			RawType: "DISABLED_CALL",
		}
	}

	if m := reCall.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    "Call " + m[1],
			RawType: "CALL",
		}
	}

	if m := reGoto.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    m[1],
			RawType: "GOTO",
		}
	}

	if m := reLabel.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    m[1],
			RawType: "LABEL",
		}
	}

	if reExit.MatchString(content) {
		return Token{
			Kind:    TokAction,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    content,
			RawType: "EXIT",
		}
	}

	if m := reErrorCapture.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    "Error => " + m[1],
			RawType: "ERROR_CAPTURE",
		}
	}

	if m := reSetVariable.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:        TokAction,
			Line:        lineNum,
			Indent:      indent,
			Raw:         raw,
			Content:     content,
			Name:        "Set " + m[1],
			RawType:     "SET",
			SetVarName:  m[1],
			SetVarValue: strings.TrimSpace(m[2]),
		}
	}

	if reWait.MatchString(content) || reWaitExpression.MatchString(content) {
		name := content
		if m := reDottedAction.FindStringSubmatch(content); m != nil {
			name = "Wait " + m[1]
		}
		return Token{
			Kind:    TokAction,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    name,
			RawType: "WAIT",
		}
	}

	if m := reDottedAction.FindStringSubmatch(content); m != nil {
		return Token{
			Kind:    TokAction,
			Line:    lineNum,
			Indent:  indent,
			Raw:     raw,
			Content: content,
			Name:    extractActionName(m[1], m[2]),
			RawType: m[1],
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
