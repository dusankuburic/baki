package parser

import (
	"sort"
	"strings"

	"pad-core/models"
)

// SerializeDocument renders a FlowDocument back to canonical PAD text: one #Region per subflow, keyword
// control flow, dotted actions with sorted Key: Value parameters. Canonical, not byte-faithful — the
// pinned contract is that output RE-PARSES equivalently: no new parse errors, same tree, same findings.
func SerializeDocument(doc *models.FlowDocument) string {
	if doc == nil {
		return ""
	}
	var sb strings.Builder
	for i := range doc.Subflows {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(SerializeSubflow(&doc.Subflows[i]))
	}
	return sb.String()
}

// SerializeFiles renders a (folder) document grouped by subflow SourceFile;
// subflows without one land under "Main.txt", and a nil/empty doc still
// yields a Main.txt key.
func SerializeFiles(doc *models.FlowDocument) map[string]string {
	files := map[string]string{}
	if doc == nil {
		return files
	}
	// Per-file Builders: concatenating onto the accumulated string re-copied
	// everything stored so far on every append.
	builders := map[string]*strings.Builder{}
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		name := sf.SourceFile
		if name == "" {
			name = "Main.txt"
		}
		b := builders[name]
		if b == nil {
			nb := &strings.Builder{}
			nb.WriteString(SerializeSubflow(sf))
			builders[name] = nb
			continue
		}
		b.WriteByte('\n')
		b.WriteString(SerializeSubflow(sf))
	}
	if len(builders) == 0 {
		files["Main.txt"] = ""
		return files
	}
	for name, b := range builders {
		files[name] = b.String()
	}
	return files
}

// SerializeSubflow renders one subflow as a #Region block (the more portable of the two observed formats; both parse).
func SerializeSubflow(sf *models.Subflow) string {
	if sf == nil {
		return ""
	}
	var sb strings.Builder
	writeLine(&sb, "", "#Region \""+sanitizeRegionName(sf.Name)+"\"")
	serializeChildList(&sb, padIndentUnit, sf.Blocks)
	writeLine(&sb, "", "#EndRegion")
	return sb.String()
}

// sanitizeRegionName keeps the region header one physical line — a multi-line name would corrupt the framing.
func sanitizeRegionName(name string) string {
	name = strings.ReplaceAll(name, "\r", " ")
	name = strings.ReplaceAll(name, "\n", " ")
	name = strings.ReplaceAll(name, "\"", "'")
	return strings.TrimSpace(name)
}

const padIndentUnit = "    "

// writeLine appends one emitted line as separate writes — the one-expression
// form allocated a fresh string per line.
func writeLine(sb *strings.Builder, ind, line string) {
	sb.WriteString(ind)
	sb.WriteString(line)
	sb.WriteByte('\n')
}

// serializeChildList emits a container's children. ELSE / CASE / DEFAULT are
// branch markers of their ENCLOSING construct: PAD prints them at the
// container's own depth with no END — the container's single END closes
// everything; every other child recurses at depth+1.
func serializeChildList(sb *strings.Builder, ind string, children []models.Block) {
	// Child indent derived once per list (every descendant shares it), not per block.
	childInd := ind + padIndentUnit
	for i := range children {
		b := &children[i]
		switch b.Type {
		case models.BlockTypeEnd:
			// Structural artifact of the parse; containers re-emit their own.
			continue
		case models.BlockTypeElse:
			// An ELSE IF rung carries its condition in Name and shares the chain's single END.
			if strings.EqualFold(b.RawType, "ELSE IF") && b.Name != "" {
				line := b.Name
				if endsWithThenSuffix(b) {
					line += " THEN"
				}
				writeLine(sb, ind, line)
			} else {
				writeLine(sb, ind, "ELSE")
			}
			serializeChildList(sb, childInd, b.Children)
		case models.BlockTypeDefault:
			writeLine(sb, ind, "DEFAULT")
			serializeChildList(sb, childInd, b.Children)
		case models.BlockTypeCase:
			// Name carries the original line ("CASE 'Active'"); empty means
			// malformed — emit the bare keyword so framing survives.
			line := b.Name
			if line == "" {
				line = "CASE"
			}
			writeLine(sb, ind, line)
			serializeChildList(sb, childInd, b.Children)
		default:
			serializeBlock(sb, childInd, b)
		}
	}
}

// serializeBlock emits one block at the given depth; stored Indent/LineNumber
// are ignored — canonical emission recomputes layout from tree depth.
func serializeBlock(sb *strings.Builder, ind string, b *models.Block) {
	childInd := ind + padIndentUnit

	if b.Type == models.BlockTypeEnd {
		return
	}

	switch {
	case b.Type == models.BlockTypeComment:
		// Doc-comment blocks (/# ... #/ spanning lines) merge into ONE token
		// whose Name is the verbatim text — emit it raw; the tokenizer's
		// inBlockComment pass re-merges identically.
		if strings.HasPrefix(b.Name, "/#") && strings.HasSuffix(b.Name, "#/") {
			writeLine(sb, ind, b.Name)
		} else if strings.Contains(b.Name, "\n") {
			// A multi-line name without doc framing cannot be one COMMENT
			// line; split per line (never occurs in real parses).
			for _, ln := range strings.Split(b.Name, "\n") {
				writeLine(sb, ind, "COMMENT  "+ln)
			}
		} else {
			// COMMENT's two-space separator is the observed PAD convention;
			// the tokenizer accepts any whitespace run.
			writeLine(sb, ind, "COMMENT  "+b.Name)
		}
		// Leaf lines can still carry children: emit them one deeper WITHOUT an
		// END — indent alone reconstructs the nesting on re-parse.
		serializeChildList(sb, childInd, b.Children)
		return

	case b.Type == models.BlockTypeErrorHandler:
		// Two parse shapes share this type: (a) plain `ON BLOCK ERROR ... END`;
		// (b) a promoted BLOCK — `BLOCK <name>` + `ON BLOCK ERROR` promotes the
		// block into the handler, leaving an OnBlockError ACTION child at the
		// marker; re-emit the BLOCK framing so re-parse promotes identically.
		if artifact := indexOfOnBlockErrorChild(b); artifact >= 0 {
			// Promotion holds TWO containers (the BLOCK and its handler
			// section) closed by two ENDs; the first BlockTypeEnd after the
			// marker is the handler/body boundary — a single END round-tripped
			// only because the old serializer and parser both discarded the body.
			writeLine(sb, ind, "BLOCK "+sanitizeRegionName(b.Name))
			serializeChildList(sb, childInd, b.Children[:artifact])
			writeLine(sb, ind, "ON BLOCK ERROR")

			handlerEnd := artifact + 1
			for handlerEnd < len(b.Children) && b.Children[handlerEnd].Type != models.BlockTypeEnd {
				handlerEnd++
			}
			serializeChildList(sb, childInd, b.Children[artifact+1:handlerEnd])
			writeLine(sb, ind, "END")

			if handlerEnd < len(b.Children) {
				serializeChildList(sb, childInd, b.Children[handlerEnd+1:])
			}
			writeLine(sb, ind, "END")
			return
		}
		writeLine(sb, ind, "ON BLOCK ERROR")
		serializeChildList(sb, childInd, b.Children)
		writeLine(sb, ind, "END")
		return

	case b.Type == models.BlockTypeSubflow && isKeywordCall(b.RawType):
		// Subflow CALLs: Name is "Call <target>" / "Call <target> (disabled)" —
		// recover the target and re-emit the CALL / DISABLE CALL form.
		target := strings.TrimPrefix(b.Name, "Call ")
		target = strings.TrimSuffix(target, " (disabled)")
		target = strings.TrimSpace(target)
		if b.RawType == "DISABLED_CALL" {
			emitLeafLine(sb, ind, "DISABLE CALL "+target, b)
		} else {
			emitLeafLine(sb, ind, "CALL "+target, b)
		}
		serializeChildList(sb, childInd, b.Children)
		return

	case b.Type == models.BlockTypeVariable && b.RawType == "SET":
		// SET preserves its RAW value (original quoting kept in _value): emit
		// verbatim, never re-quoted; a missing _var/_value degrades to a parse
		// warning rather than corrupting the region framing.
		emitLeafLine(sb, ind, "SET "+b.Properties["_var"]+" TO "+b.Properties["_value"], b)
		serializeChildList(sb, childInd, b.Children)
		return

	case b.RawType == "ERROR_CAPTURE":
		// `ERROR => LastError Reset: True`: the output variable lives in Name
		// ("Error => LastError"), NOT _output — reconstruct from Name + the
		// non-internal properties.
		out := b.Properties["_output"]
		if out == "" {
			if i := strings.Index(b.Name, "=>"); i >= 0 {
				out = strings.TrimSpace(b.Name[i+len("=>"):])
			}
		}
		line := "ERROR"
		if out != "" {
			line += " => " + out
		}
		line += serializePropsSuffix(b)
		emitLeafLine(sb, ind, line, b)
		serializeChildList(sb, childInd, b.Children)
		return

	case isFlowKeywordAction(b.RawType):
		// Source keyword forms differ from the normalized RawType:
		// EXIT_LOOP/NEXT_LOOP are two words, GOTO/LABEL carry a bare target in
		// Name, EXIT keeps its full content in Name (e.g. "EXIT Subflow").
		switch b.RawType {
		case "EXIT_LOOP":
			emitLeafLine(sb, ind, "EXIT LOOP", b)
		case "NEXT_LOOP":
			emitLeafLine(sb, ind, "NEXT LOOP", b)
		case "GOTO", "LABEL":
			emitLeafLine(sb, ind, b.RawType+" "+b.Name, b)
		case "EXIT":
			if b.Name != "" {
				emitLeafLine(sb, ind, b.Name, b)
			} else {
				emitLeafLine(sb, ind, "EXIT", b)
			}
		default:
			emitLeafLine(sb, ind, b.RawType, b)
		}
		serializeChildList(sb, childInd, b.Children)
		return

	case b.RawType == "REGION":
		// Star-region framing (`**REGION NAME` / `**ENDREGION`) parses as a
		// BLOCK with RawType REGION; a bare REGION line re-parses as malformed.
		writeLine(sb, ind, "**REGION "+b.Name)
		serializeChildList(sb, childInd, b.Children)
		writeLine(sb, ind, "**ENDREGION")
		return

	case isKeywordControl(b.Type, b.RawType):
		// IF/LOOP/SWITCH/WAIT/BLOCK keyword forms: Name carries the ORIGINAL
		// line content, emitted verbatim EXCEPT the optional trailing THEN —
		// the tokenizer strips it from Name but the value scan ABSORBED it into
		// the last value; re-append so re-parse regenerates identical properties.
		line := b.Name
		if line == "" {
			line = b.RawType
		}
		if endsWithThenSuffix(b) {
			line += " THEN"
		}
		emitLeafLine(sb, ind, line, b)
		serializeChildList(sb, childInd, b.Children)
		if b.Type != models.BlockTypeWait {
			// WAIT is the one keyword block the parser does NOT END-pop: it
			// closes implicitly by indent. An END after WAIT's children would
			// steal the enclosing construct's closure, re-parenting a following
			// ELSE to the wrong level.
			writeLine(sb, ind, "END")
		}
		return

	default:
		// Everything else (leaf ACTIONs, dotted VARIABLE ops, dotted If/Loop
		// containers) uses `<RawType> Key: Value ... => Output`. Dotted
		// containers close with END; leaf blocks that merely carry indented
		// followers emit children with NO END — indent alone reconstructs the nesting.
		emitLeafLine(sb, ind, serializeGenericAction(b), b)
		serializeChildList(sb, childInd, b.Children)
		if isContainerType(b.Type) {
			writeLine(sb, ind, "END")
		}
	}
}

// indexOfOnBlockErrorChild returns the OnBlockError ACTION artifact child position (promoted-BLOCK marker), or -1.
func indexOfOnBlockErrorChild(b *models.Block) int {
	for i := range b.Children {
		c := &b.Children[i]
		if c.RawType == "OnBlockError" && c.Type != models.BlockTypeErrorHandler {
			return i
		}
	}
	return -1
}

// serializePropsSuffix renders ` Key: Value` for non-internal properties (ERROR_CAPTURE line).
func serializePropsSuffix(b *models.Block) string {
	keys := make([]string, 0, len(b.Properties))
	for k := range b.Properties {
		if strings.HasPrefix(k, "_") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteByte(' ')
		sb.WriteString(k)
		sb.WriteString(": ")
		sb.WriteString(QuoteValue(b.Properties[k]))
	}
	return sb.String()
}

// endsWithThenSuffix reports whether any property value ends with " THEN" —
// the fingerprint of an `IF ... THEN` line whose trailing keyword the
// tokenizer stripped from Name but the value scanner absorbed.
func endsWithThenSuffix(b *models.Block) bool {
	for _, v := range b.Properties {
		if strings.HasSuffix(v, " THEN") {
			return true
		}
	}
	return false
}

// emitLeafLine writes one leaf block line followed, when the block carries
// them, by its INLINE RETRY directive — the parser folds it into _retry*
// properties, and dropping it on re-emit silently changed analysis results
// (missing-timeout matches the "wait" in _retryWait).
func emitLeafLine(sb *strings.Builder, ind, line string, b *models.Block) {
	writeLine(sb, ind, line)
	if b.Properties["_retryCount"] == "" {
		return
	}
	d := "ON ERROR REPEAT " + b.Properties["_retryCount"] + " TIMES WAIT " + b.Properties["_retryWait"]
	if v := b.Properties["_retryType"]; v != "" {
		d += " RetryType: " + v
	}
	if v := b.Properties["_retryMinInterval"]; v != "" {
		d += " MinInterval: " + v
	}
	if v := b.Properties["_retryMaxInterval"]; v != "" {
		d += " MaxInterval: " + v
	}
	writeLine(sb, ind, d)
}

func isKeywordCall(raw string) bool {
	return raw == "CALL" || raw == "DISABLED_CALL"
}

// isFlowKeywordAction covers the zero-argument flow primitives classified as leaf ACTIONs by the classifier's exact map.
func isFlowKeywordAction(raw string) bool {
	switch raw {
	case "GOTO", "LABEL", "EXIT", "EXIT_LOOP", "NEXT_LOOP", "ERROR_CAPTURE":
		return true
	}
	return false
}

// isKeywordControl reports whether the block parsed from a keyword-style control line (Name preserves the full original text).
func isKeywordControl(t models.BlockType, raw string) bool {
	switch t {
	case models.BlockTypeCondition, models.BlockTypeLoop, models.BlockTypeSwitch,
		models.BlockTypeWait, models.BlockTypeBlock:
		// Dotted variants (Text.If, Loop.ForEach, ...) carry their condition
		// in Properties and must use the generic path.
		return !strings.Contains(raw, ".")
	}
	return false
}

// isContainerType reports whether a block nests children in the generic (dotted) path; CASE/DEFAULT/ELSE live in serializeChildList.
func isContainerType(t models.BlockType) bool {
	switch t {
	case models.BlockTypeCondition, models.BlockTypeLoop, models.BlockTypeSwitch,
		models.BlockTypeBlock:
		return true
	}
	return false
}

// serializeGenericAction renders `<RawType> Key: Value ... => Out` with
// sorted keys (map iteration is random; round-trip tests rely on determinism).
// Internal `_`-prefixed keys are dropped except `_output`, the => suffix.
func serializeGenericAction(b *models.Block) string {
	var sb strings.Builder
	sb.WriteString(b.RawType)
	keys := make([]string, 0, len(b.Properties))
	for k := range b.Properties {
		if strings.HasPrefix(k, "_") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sb.WriteByte(' ')
		sb.WriteString(k)
		sb.WriteString(": ")
		sb.WriteString(QuoteValue(b.Properties[k]))
	}
	if out := b.Properties["_output"]; out != "" {
		sb.WriteString(" => ")
		sb.WriteString(out)
	}
	return sb.String()
}

// QuoteValue re-quotes a parsed (unwrapped) property value for emission:
// bare iff it contains no whitespace and no quotes of any kind; everything
// else wraps in a dollar-prefixed triple-quote literal — the parser strips
// that wrapper back off, so the decision need not match the original file's.
func QuoteValue(v string) string {
	if v == "" {
		// Six single quotes: a triple-quoted empty literal — the shortest form stripQuotes unwraps.
		return "''''''"
	}
	if strings.Contains(v, "'''") {
		// The PARSED value already embeds quote markers (some values scan
		// through a quoted section plus trailing words); no wrapper can safely
		// contain a triple-quote — emitting VERBATIM reproduces the original
		// token scan exactly.
		return v
	}
	if !strings.ContainsAny(v, " \t\r\n'\"") {
		return v
	}
	return "$'''" + v + "'''"
}
