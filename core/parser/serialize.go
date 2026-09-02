package parser

import (
	"sort"
	"strings"

	"pad-core/models"
)

// SerializeDocument renders a FlowDocument back to canonical PAD text: one
// #Region per subflow, keyword-style control flow, dotted-style actions with
// sorted Key: Value parameters. It is the parser's inverse for the paths this
// project produces and consumes — ingested cloud flows (no stored source) can
// be fixed/exported, and structured editors can emit PAD text.
//
// Canonical, not byte-faithful: original comment spacing, blank lines, and
// parameter order are normalized (properties are a map). The correctness
// contract — pinned by the SerializeRoundTrip tests — is that the output
// RE-PARSES into an equivalent document: no new parse errors, the same block
// tree (type/rawType/properties/nesting), and the same analysis findings.
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

// SerializeFiles renders a (folder) document grouped by subflow SourceFile:
// map[filename]content, mirroring the multi-file layout ParseFiles consumed.
// Subflows without a SourceFile land under "Main.txt". A nil/empty doc yields
// a single empty Main.txt entry so callers can rely on the key existing.
func SerializeFiles(doc *models.FlowDocument) map[string]string {
	files := map[string]string{}
	if doc == nil {
		return files
	}
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		name := sf.SourceFile
		if name == "" {
			name = "Main.txt"
		}
		if prev, ok := files[name]; ok {
			files[name] = prev + "\n" + SerializeSubflow(sf)
			continue
		}
		files[name] = SerializeSubflow(sf)
	}
	if len(files) == 0 {
		files["Main.txt"] = ""
	}
	return files
}

// SerializeSubflow renders one subflow as a #Region block. The region style
// is the most portable of the two observed subflow formats (both parse; the
// dotted style is newer and less uniformly supported downstream).
func SerializeSubflow(sf *models.Subflow) string {
	if sf == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("#Region \"" + sanitizeRegionName(sf.Name) + "\"\n")
	serializeChildList(&sb, 1, sf.Blocks)
	sb.WriteString("#EndRegion\n")
	return sb.String()
}

// sanitizeRegionName keeps the region header a single physical line — a
// multi-line subflow name would otherwise corrupt the region framing.
func sanitizeRegionName(name string) string {
	name = strings.ReplaceAll(name, "\r", " ")
	name = strings.ReplaceAll(name, "\n", " ")
	name = strings.ReplaceAll(name, "\"", "'")
	return strings.TrimSpace(name)
}

const padIndentUnit = "    "

// serializeChildList emits a container's children. ELSE / CASE / DEFAULT are
// branch markers of their ENCLOSING construct: PAD prints them at the
// container's own depth (not one deeper), and they carry no END — the
// container's single END closes everything. Every other child recurses at
// depth+1.
func serializeChildList(sb *strings.Builder, depth int, children []models.Block) {
	ind := strings.Repeat(padIndentUnit, depth)
	for i := range children {
		b := &children[i]
		switch b.Type {
		case models.BlockTypeEnd:
			// Structural artifact of the parse; containers re-emit their own.
			continue
		case models.BlockTypeElse:
			sb.WriteString(ind + "ELSE\n")
			serializeChildList(sb, depth+1, b.Children)
		case models.BlockTypeDefault:
			sb.WriteString(ind + "DEFAULT\n")
			serializeChildList(sb, depth+1, b.Children)
		case models.BlockTypeCase:
			// Name carries the original line ("CASE 'Active'"); empty name is
			// a malformed parse — emit the bare keyword so framing survives.
			line := b.Name
			if line == "" {
				line = "CASE"
			}
			sb.WriteString(ind + line + "\n")
			serializeChildList(sb, depth+1, b.Children)
		default:
			serializeBlock(sb, depth+1, b)
		}
	}
}

// serializeBlock emits one block at the given depth. Stored Indent/LineNumber
// are ignored — canonical emission recomputes layout from tree depth.
func serializeBlock(sb *strings.Builder, depth int, b *models.Block) {
	ind := strings.Repeat(padIndentUnit, depth)

	if b.Type == models.BlockTypeEnd {
		return
	}

	switch {
	case b.Type == models.BlockTypeComment:
		// Doc-comment blocks (/# ... #/ spanning lines) merge into ONE token
		// whose Name is the verbatim text — emit it raw at the block's
		// indent; the tokenizer's inBlockComment pass re-merges identically.
		if strings.HasPrefix(b.Name, "/#") && strings.HasSuffix(b.Name, "#/") {
			sb.WriteString(ind + b.Name + "\n")
		} else if strings.Contains(b.Name, "\n") {
			// A multi-line name without doc-comment framing cannot be one
			// COMMENT line; split per line (degrades to N comments — this
			// shape doesn't occur in real parses, which use /#...#/).
			for _, ln := range strings.Split(b.Name, "\n") {
				sb.WriteString(ind + "COMMENT  " + ln + "\n")
			}
		} else {
			// COMMENT's two-space separator is the observed PAD convention;
			// the tokenizer accepts any run of whitespace.
			sb.WriteString(ind + "COMMENT  " + b.Name + "\n")
		}
		// Leaf lines can still carry children (the parser nests indented
		// followers under any block): emit them one deeper WITHOUT an END —
		// indent alone reconstructs the nesting on re-parse.
		serializeChildList(sb, depth+1, b.Children)
		return

	case b.Type == models.BlockTypeErrorHandler:
		// Two parse shapes share this type:
		//
		//  (a) plain `ON BLOCK ERROR ... END` — Name is a friendly label
		//      ("On Block Error"); emit the canonical keyword.
		//  (b) promoted BLOCK: `BLOCK <name>` + `ON BLOCK ERROR` promoted the
		//      block into the handler, leaving an OnBlockError ACTION child
		//      at the marker's position. Name preserves the BLOCK's name.
		//      Emit the original BLOCK framing so re-parse promotes again
		//      into the identical tree.
		if artifact := indexOfOnBlockErrorChild(b); artifact >= 0 {
			// Promotion MERGES the BLOCK and the handler into one construct:
			// re-parse closes BOTH with a single END (skipNextEnd pops the
			// promoted handler). Two ENDs would over-close into the parent.
			sb.WriteString(ind + "BLOCK " + sanitizeRegionName(b.Name) + "\n")
			serializeChildList(sb, depth+1, b.Children[:artifact])
			sb.WriteString(ind + padIndentUnit + "ON BLOCK ERROR\n")
			serializeChildList(sb, depth+2, b.Children[artifact+1:])
			sb.WriteString(ind + padIndentUnit + "END\n")
			return
		}
		sb.WriteString(ind + "ON BLOCK ERROR\n")
		serializeChildList(sb, depth+1, b.Children)
		sb.WriteString(ind + "END\n")
		return

	case b.Type == models.BlockTypeSubflow && isKeywordCall(b.RawType):
		// Subflow CALLs: Name is "Call <target>" / "Call <target> (disabled)"
		// — recover the target and re-emit the CALL / DISABLE CALL form.
		target := strings.TrimPrefix(b.Name, "Call ")
		target = strings.TrimSuffix(target, " (disabled)")
		target = strings.TrimSpace(target)
		if b.RawType == "DISABLED_CALL" {
			emitLeafLine(sb, ind, "DISABLE CALL "+target, b)
		} else {
			emitLeafLine(sb, ind, "CALL "+target, b)
		}
		serializeChildList(sb, depth+1, b.Children)
		return

	case b.Type == models.BlockTypeVariable && b.RawType == "SET":
		// SET preserves its RAW value (original quoting kept in _value by the
		// parser) — emit verbatim, never re-quoted. A missing _var/_value is
		// a malformed parse; the empty emission degrades to a parse warning
		// rather than corrupting the region framing.
		emitLeafLine(sb, ind, "SET "+b.Properties["_var"]+" TO "+b.Properties["_value"], b)
		serializeChildList(sb, depth+1, b.Children)
		return

	case b.RawType == "ERROR_CAPTURE":
		// `ERROR => LastError Reset: True`: the output variable lives in
		// Name ("Error => LastError"), NOT in _output. Reconstruct the line
		// from Name + the non-internal properties.
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
		serializeChildList(sb, depth+1, b.Children)
		return

	case isFlowKeywordAction(b.RawType):
		// Source keyword forms differ from the normalized RawType:
		// EXIT_LOOP/NEXT_LOOP are two words ("EXIT LOOP" / "NEXT LOOP");
		// GOTO/LABEL carry a bare target in Name; EXIT keeps its full
		// content in Name (e.g. "EXIT Subflow").
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
		serializeChildList(sb, depth+1, b.Children)
		return

	case b.RawType == "REGION":
		// Star-region framing (`**REGION NAME` / `**ENDREGION`) parses as a
		// BLOCK with RawType REGION; re-emit the star form (a bare REGION
		// line re-parses as malformed).
		sb.WriteString(ind + "**REGION " + b.Name + "\n")
		serializeChildList(sb, depth+1, b.Children)
		sb.WriteString(ind + "**ENDREGION\n")
		return

	case isKeywordControl(b.Type, b.RawType):
		// IF/LOOP/SWITCH/WAIT/BLOCK keyword forms: Name carries the ORIGINAL
		// line content ("IF %x% = OK", "WAIT 5") — emit verbatim, EXCEPT the
		// optional trailing THEN: the tokenizer strips it from Name, but the
		// property-value scan ran to end-of-line and ABSORBED it into the
		// last value. Re-append when any prop value still ends with " THEN"
		// so the re-parse regenerates identical properties.
		line := b.Name
		if line == "" {
			line = b.RawType
		}
		if endsWithThenSuffix(b) {
			line += " THEN"
		}
		emitLeafLine(sb, ind, line, b)
		serializeChildList(sb, depth+1, b.Children)
		if b.Type != models.BlockTypeWait {
			// WAIT is the one keyword block the parser does NOT END-pop
			// (handleEnd's list is Loop/Condition/ErrorHandler/Block/Switch):
			// it closes implicitly by indent. An END after WAIT's children
			// would fall through to the enclosing IF/LOOP and steal ITS
			// closure — re-parenting a following ELSE to the wrong level.
			sb.WriteString(ind + "END\n")
		}
		return

	default:
		// Everything else (leaf ACTIONs, dotted VARIABLE ops, dotted If/Loop
		// containers) uses the generic dotted form:
		//     <RawType> Key: Value ... => Output
		// Dotted containers (classifier prefix rules) close with END; leaf
		// blocks that merely carry indented followers (the parser nests by
		// indent under ANY block) emit children with NO END — indent alone
		// reconstructs them.
		emitLeafLine(sb, ind, serializeGenericAction(b), b)
		serializeChildList(sb, depth+1, b.Children)
		if isContainerType(b.Type) {
			sb.WriteString(ind + "END\n")
		}
	}
}

// indexOfOnBlockErrorChild returns the position of the OnBlockError ACTION
// artifact child (the promoted-BLOCK parse marker), or -1.
func indexOfOnBlockErrorChild(b *models.Block) int {
	for i := range b.Children {
		c := &b.Children[i]
		if c.RawType == "OnBlockError" && c.Type != models.BlockTypeErrorHandler {
			return i
		}
	}
	return -1
}

// serializePropsSuffix renders ` Key: Value` for the non-internal properties
// (no leading RawType, no _output suffix) — used by the ERROR_CAPTURE line.
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
		sb.WriteString(" " + k + ": " + QuoteValue(b.Properties[k]))
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
// them, by its INLINE RETRY directive (`ON ERROR REPEAT n TIMES WAIT w ...`).
// The parser folds that directive into the parent's _retry* properties —
// dropping it on re-emit silently changed analysis results (hasTimeoutConfigured
// matches the "wait" in _retryWait, so missing-timeout findings moved).
func emitLeafLine(sb *strings.Builder, ind, line string, b *models.Block) {
	sb.WriteString(ind + line + "\n")
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
	sb.WriteString(ind + d + "\n")
}

func isKeywordCall(raw string) bool {
	return raw == "CALL" || raw == "DISABLED_CALL"
}

// isFlowKeywordAction covers the zero-argument flow primitives classified as
// leaf ACTIONs by the classifier's exact map.
func isFlowKeywordAction(raw string) bool {
	switch raw {
	case "GOTO", "LABEL", "EXIT", "EXIT_LOOP", "NEXT_LOOP", "ERROR_CAPTURE":
		return true
	}
	return false
}

// isKeywordControl reports whether the block was parsed from a keyword-style
// control line (whose Name preserves the full original line text).
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

// isContainerType reports whether a block nests children in the generic
// (dotted) emission path. CASE/DEFAULT/ELSE are handled in serializeChildList.
func isContainerType(t models.BlockType) bool {
	switch t {
	case models.BlockTypeCondition, models.BlockTypeLoop, models.BlockTypeSwitch,
		models.BlockTypeBlock:
		return true
	}
	return false
}

// serializeGenericAction renders `<RawType> Key: Value ... => Out` with
// sorted property keys (map iteration is random; sorted output is
// deterministic, which round-trip tests rely on). Internal `_`-prefixed
// keys are dropped except `_output`, which becomes the => suffix.
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
		sb.WriteString(" " + k + ": " + QuoteValue(b.Properties[k]))
	}
	if out := b.Properties["_output"]; out != "" {
		sb.WriteString(" => " + out)
	}
	return sb.String()
}

// QuoteValue re-quotes a parsed (unwrapped) property value for emission.
// Bare iff it contains no whitespace and no quotes of any kind — a bare
// value terminates at whitespace, and a quote character would start a quoted
// section mid-value. Everything else is wrapped in a $”'...”' literal; the
// parser strips that wrapper back off, so the quoting decision doesn't need
// to match the original file's.
func QuoteValue(v string) string {
	if v == "" {
		// Six single quotes: a triple-quoted empty literal — the shortest
		// form stripQuotes unwraps (its len>5 guard).
		return "''''''"
	}
	if strings.Contains(v, "'''") {
		// The PARSED value already embeds quote markers (values like
		// `$'''text''' Result` scan through a quoted section plus trailing
		// words). No wrapper can safely contain ''' — emitting the value
		// VERBATIM reproduces the original token scan exactly.
		return v
	}
	if !strings.ContainsAny(v, " \t\r\n'\"") {
		return v
	}
	return "$'''" + v + "'''"
}
