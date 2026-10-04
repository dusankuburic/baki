package parser

import (
	"strings"

	"pad-core/models"
)

func parseProperties(raw string) map[string]string {
	props := make(map[string]string)

	if m := reOutputVar.FindStringSubmatch(raw); m != nil {
		props["_output"] = m[1]
	}

	cleaned := reOutputVar.ReplaceAllString(raw, "")
	cleaned = strings.TrimSpace(cleaned)

	// No colon anywhere ⇒ no key: value pair can exist; skip the scan (most
	// action lines have no properties at all).
	if !strings.Contains(cleaned, ":") {
		return props
	}

	props = extractKeyValuePairs(cleaned, props)

	return props
}

// The scanners below are byte-wise on purpose: every delimiter and key char
// is ASCII, and UTF-8 continuation bytes (≥0x80) fail isKeyChar just like the
// multibyte runes they form — byte positions behave identically to rune
// positions for what is found, without the []rune allocation.

func extractKeyValuePairs(text string, props map[string]string) map[string]string {
	pos := 0

	for pos < len(text) {
		key, keyEnd := findNextKey(text, pos)
		if key == "" {
			break
		}

		valStart := keyEnd
		valEnd, nextPos := findValueEnd(text, valStart)

		value := strings.TrimSpace(text[valStart:valEnd])
		value = stripQuotes(value)
		props[key] = value

		pos = nextPos
	}

	return props
}

// findNextKey scans forward for the next `Key: ` boundary and returns the key
// plus the offset just past the colon.
//
// On a failed match it resumes at the END of the key-character run — not just
// an optimisation but the only correct resumption point: a key is a MAXIMAL
// run followed by ':' and a space/tab/EOL, so if the full run is not a key no
// sub-run can be either. Resuming at keyStart+1 instead re-scanned the run on
// every byte (N²/2 comparisons; a 2 MB line took tens of minutes of CPU —
// TestParseProperties_UnterminatedKeyRunIsLinear).
func findNextKey(text string, start int) (string, int) {
	pos := start
	for pos < len(text) {
		if !isKeyChar(text[pos]) {
			pos++
			continue
		}

		keyStart := pos
		for pos < len(text) && isKeyChar(text[pos]) {
			pos++
		}
		runEnd := pos

		if runEnd >= len(text) || text[runEnd] != ':' {
			pos = runEnd
			continue
		}
		pos = runEnd + 1

		if pos >= len(text) || text[pos] == ' ' || text[pos] == '\t' {
			return text[keyStart:runEnd], pos
		}

		// Colon present but not a key boundary. Resume past the colon — the run
		// itself is already excluded by the argument above.
		pos = runEnd + 1
	}
	return "", pos
}

func findValueEnd(text string, start int) (int, int) {
	pos := start

	for pos < len(text) {
		if text[pos] == ' ' || text[pos] == '\t' {
			pos++
			continue
		}
		break
	}

	for pos < len(text) {
		ch := text[pos]

		if ch == '$' && pos+3 < len(text) && text[pos+1] == '\'' && text[pos+2] == '\'' && text[pos+3] == '\'' {
			end := findTripleQuoteEnd(text, pos+4)
			if end == -1 {
				pos = len(text)
			} else {
				pos = end
			}
			continue
		}

		if ch == '\'' && pos+2 < len(text) && text[pos+1] == '\'' && text[pos+2] == '\'' {
			end := findTripleQuoteEnd(text, pos+3)
			if end == -1 {
				pos = len(text)
			} else {
				pos = end
			}
			continue
		}

		if isKeyChar(ch) {
			peek := pos + 1
			for peek < len(text) && isKeyChar(text[peek]) {
				peek++
			}
			if peek < len(text) && text[peek] == ':' {
				nextAfterColon := peek + 1
				if nextAfterColon >= len(text) || text[nextAfterColon] == ' ' || text[nextAfterColon] == '\t' {
					return pos, pos
				}
			}
			// Not a key boundary. Skip the ENTIRE run just scanned — every
			// sub-run ends at the same `peek`, so none can be a boundary
			// either. Advancing by one made this O(n²) (see findNextKey).
			pos = peek
			continue
		}

		pos++
	}

	return pos, pos
}

func findTripleQuoteEnd(text string, start int) int {
	for i := start; i+2 < len(text); i++ {
		if text[i] == '\'' && text[i+1] == '\'' && text[i+2] == '\'' {
			return i + 3
		}
	}
	return -1
}

func isKeyChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '.'
}

func stripQuotes(s string) string {
	if strings.HasPrefix(s, "$'''") && strings.HasSuffix(s, "'''") && len(s) > 6 {
		return s[4 : len(s)-3]
	}
	if strings.HasPrefix(s, "'''") && strings.HasSuffix(s, "'''") && len(s) > 5 {
		return s[3 : len(s)-3]
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func extractVariables(raw string) []string {
	// Mask %% escape sequences so they are not matched as variable delimiters.
	cleaned := strings.ReplaceAll(raw, "%%", "\x00\x00")
	matches := reVariableRef.FindAllStringSubmatch(cleaned, -1)
	seen := make(map[string]bool)
	vars := make([]string, 0)

	for _, m := range matches {
		expression := m[1]
		// Mask string literals to avoid extracting identifiers from them.
		masked := maskStrings(expression)

		// Every index below is read from — and applied back onto — masked,
		// never expression: maskStrings collapses each literal to a single
		// space, so the two strings' lengths diverge and an offset computed
		// against one must never slice the other. reIdentifier matches only
		// ASCII word chars, never altered outside literals, so masked[start:end]
		// holds the identical identifier text.
		ids := reIdentifier.FindAllStringSubmatchIndex(masked, -1)
		for _, idMatch := range ids {
			start, end := idMatch[0], idMatch[1]
			vname := masked[start:end]

			// Check if it's a property (preceded by a dot).
			if start > 0 && masked[start-1] == '.' {
				continue
			}

			// Check if it's a function call (followed by an open paren).
			isFunc := false
			for i := end; i < len(masked); i++ {
				if masked[i] == ' ' || masked[i] == '\t' {
					continue
				}
				if masked[i] == '(' {
					isFunc = true
				}
				break
			}
			if isFunc {
				continue
			}

			// Skip logical operators and constants.
			if isExpressionKeyword(vname) {
				continue
			}
			if !seen[vname] {
				seen[vname] = true
				vars = append(vars, vname)
			}
		}
	}
	return vars
}

// normalizeVariableOutput ensures variable-manipulation blocks writing a
// named output carry Properties["_output"], so rules track written variables uniformly.
func normalizeVariableOutput(blk *models.Block) {
	switch blk.RawType {
	case "Variables.SetVariable",
		"Variables.IncreaseVariable",
		"Variables.DecreaseVariable",
		"Variables.AddItemToList",
		"Variables.RemoveItemFromList",
		"Variables.SortList",
		"Variables.ShuffleList",
		"Variables.MergeLists",
		"Variables.ReverseList",
		"Variables.RemoveDuplicateItemsFromList",
		"Variables.FindCommonListItems",
		"Variables.SubtractLists",
		"Variables.ClearList":
		// The "Name" parameter holds the variable being modified.
		// Strip %...% wrapper if PAD emitted it in that form; skip compound expressions.
		if name, ok := blk.Properties["Name"]; ok && name != "" {
			bare := name
			if strings.HasPrefix(bare, "%") && strings.HasSuffix(bare, "%") && len(bare) > 2 {
				bare = bare[1 : len(bare)-1]
			}
			if !strings.Contains(bare, "%") && bare != "" {
				blk.Properties["_output"] = bare
			}
		}
	}
}
