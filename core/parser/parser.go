package parser

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"pad-core/models"

	"github.com/google/uuid"
)

const maxNestingDepth = 200

type stackEntry struct {
	indent int
	block  *models.Block
}

type builtSubflow struct {
	id    string
	name  string
	roots []*models.Block
}

type parseState struct {
	built       []*builtSubflow
	current     *builtSubflow
	parseErrors []models.ParseError
	stack       []stackEntry
	skipNextEnd bool
}

func newParseState() *parseState {
	return &parseState{}
}

// processToken dispatches one Token to the handler for its Kind.
func (s *parseState) processToken(tok Token) {
	switch tok.Kind {
	case TokEmpty:
		return
	case TokError:
		s.recordMalformedLine(tok)
	case TokSubflowStart:
		s.handleSubflowStart(tok)
	case TokSubflowEnd:
		s.handleSubflowEnd(tok)
	case TokErrorHandlerStart:
		s.handleErrorHandlerStart(tok)
	case TokBlockStart:
		s.handleBlockStart(tok)
	case TokSwitchStart:
		s.handleSwitchStart(tok)
	case TokCase:
		s.handleCase(tok)
	case TokDefault:
		s.handleDefault(tok)
	case TokLoopStart:
		s.handleLoopStart(tok)
	case TokEnd:
		s.handleEnd(tok)
	case TokConditionStart:
		s.handleConditionStart(tok)
	case TokConditionElse:
		s.handleConditionElse(tok)
	case TokComment:
		s.handleComment(tok)
	case TokAction:
		s.handleAction(tok)
	}
}

// requireSubflow records a "<kind> outside subflow" parse error and returns false when outside a subflow.
func (s *parseState) requireSubflow(tok Token, kind string) bool {
	if s.current != nil {
		return true
	}
	s.parseErrors = append(s.parseErrors, models.ParseError{
		Line: tok.Line, Column: tok.Column, Message: kind + " outside subflow",
		Severity: "warning", Snippet: truncate(tok.Raw, 200),
	})
	return false
}

// stackHas reports whether any stack entry has the given block type; the
// CASE/DEFAULT/ELSE handlers must check before popping — unconditional popping
// when the parent type was absent cleared the entire stack, orphaning every
// subsequent sibling at the root.
func (s *parseState) stackHas(t models.BlockType) bool {
	for _, e := range s.stack {
		if e.block.Type == t {
			return true
		}
	}
	return false
}

// requireDepth reports whether the nesting stack has room for one more level,
// else records a "nesting depth exceeds N" parse error; flat kinds
// (CASE/DEFAULT/ELSE) reuse a level.
func (s *parseState) requireDepth(tok Token) bool {
	if len(s.stack) < maxNestingDepth {
		return true
	}
	s.parseErrors = append(s.parseErrors, models.ParseError{
		Line: tok.Line, Column: tok.Column, Message: fmt.Sprintf("nesting depth exceeds %d", maxNestingDepth),
		Severity: "error", Snippet: truncate(tok.Raw, 200),
	})
	return false
}

func (s *parseState) recordMalformedLine(tok Token) {
	// An unterminated multi-line span is not an ordinary unclassifiable line:
	// an unclosed /# or triple-quote consumed every line after it — report at
	// error severity, the severity CountParseErrors counts (CI gate, fix-loop
	// regression guard).
	if tok.RawType == "UNTERMINATED" {
		s.parseErrors = append(s.parseErrors, models.ParseError{
			Line: tok.Line, Column: tok.Column, Message: tok.Name,
			Severity: "error", Snippet: truncate(tok.Raw, 200),
		})
		return
	}
	s.parseErrors = append(s.parseErrors, models.ParseError{
		Line: tok.Line, Column: tok.Column, Message: "malformed line",
		Severity: "warning", Snippet: truncate(tok.Raw, 200),
	})
}

func (s *parseState) handleSubflowStart(tok Token) {
	// Record unclosed blocks before starting a new subflow (mirrors
	// handleSubflowEnd): a missing EndRegion would otherwise silently drop
	// all open IF/LOOP/BLOCK entries.
	s.recordUnclosedBlocks()
	s.current = &builtSubflow{id: uuid.NewString(), name: tok.Name}
	s.built = append(s.built, s.current)
	s.stack = nil
}

func (s *parseState) handleSubflowEnd(Token) {
	s.recordUnclosedBlocks()
	s.current = nil
	s.stack = nil
}

// recordUnclosedBlocks flags every END-closable block still open on the stack
// before a subflow boundary clears it, so a missing END isn't silently
// dropped. Leaf entries live on the stack between siblings and were popped by
// popStack, so flagging them produced spurious errors.
func (s *parseState) recordUnclosedBlocks() {
	for _, entry := range s.stack {
		t := entry.block.Type
		if t != models.BlockTypeLoop && t != models.BlockTypeCondition &&
			t != models.BlockTypeErrorHandler && t != models.BlockTypeBlock &&
			t != models.BlockTypeSwitch {
			continue
		}
		s.parseErrors = append(s.parseErrors, models.ParseError{
			Line:     entry.block.LineNumber,
			Message:  fmt.Sprintf("unclosed block: %s", entry.block.Name),
			Severity: "error",
			Snippet:  entry.block.RawType,
		})
	}
}

func (s *parseState) handleErrorHandlerStart(tok Token) {
	if !s.requireSubflow(tok, "error handler") {
		return
	}
	if len(s.stack) > 0 && s.stack[len(s.stack)-1].block.Type == models.BlockTypeBlock {
		top := s.stack[len(s.stack)-1].block
		top.Type = models.BlockTypeErrorHandler
		top.RawType = "OnBlockError"

		// Add the ON BLOCK ERROR line as a child so it's visible in the UI
		blk := newBlock(tok, s.current.id, models.BlockTypeAction)
		blk.ParentID = top.ID
		top.ChildPtrs = append(top.ChildPtrs, blk)

		s.skipNextEnd = true
		return
	}
	// The depth guard sits HERE, not at the top like its peers: the early-return path above converts an
	// accepted BLOCK into an error handler and pushes nothing, so a whole-function guard would reject that
	// conversion at max depth — and unguarded, nested ON BLOCK ERROR could exceed maxNestingDepth.
	if !s.requireDepth(tok) {
		return
	}
	blk := newBlock(tok, s.current.id, models.BlockTypeErrorHandler)
	s.stack = popStack(s.stack, blk.Indent)
	s.stack = insertIntoTree(s.current, blk, s.stack)
}

func (s *parseState) handleBlockStart(tok Token) {
	if !s.requireSubflow(tok, "block") || !s.requireDepth(tok) {
		return
	}
	blk := newBlock(tok, s.current.id, models.BlockTypeBlock)
	s.stack = popStack(s.stack, blk.Indent)
	s.stack = insertIntoTree(s.current, blk, s.stack)
}

func (s *parseState) handleSwitchStart(tok Token) {
	if !s.requireSubflow(tok, "switch") || !s.requireDepth(tok) {
		return
	}
	blk := newBlock(tok, s.current.id, models.BlockTypeSwitch)
	s.stack = popStack(s.stack, blk.Indent)
	s.stack = insertIntoTree(s.current, blk, s.stack)
}

func (s *parseState) handleCase(tok Token) {
	if s.current == nil {
		return
	}
	if !s.stackHas(models.BlockTypeSwitch) {
		s.parseErrors = append(s.parseErrors, models.ParseError{
			Line: tok.Line, Column: tok.Column, Message: "CASE outside SWITCH",
			Severity: "warning", Snippet: truncate(tok.Raw, 200),
		})
		return
	}
	for s.stack[len(s.stack)-1].block.Type != models.BlockTypeSwitch {
		s.stack = s.stack[:len(s.stack)-1]
	}
	caseBlock := newBlock(tok, s.current.id, models.BlockTypeCase)
	parent := s.stack[len(s.stack)-1].block
	caseBlock.ParentID = parent.ID
	parent.ChildPtrs = append(parent.ChildPtrs, caseBlock)
	s.stack = append(s.stack, stackEntry{indent: tok.Indent + 1, block: caseBlock})
}

func (s *parseState) handleDefault(tok Token) {
	if s.current == nil {
		return
	}
	if !s.stackHas(models.BlockTypeSwitch) {
		s.parseErrors = append(s.parseErrors, models.ParseError{
			Line: tok.Line, Column: tok.Column, Message: "DEFAULT outside SWITCH",
			Severity: "warning", Snippet: truncate(tok.Raw, 200),
		})
		return
	}
	for s.stack[len(s.stack)-1].block.Type != models.BlockTypeSwitch {
		s.stack = s.stack[:len(s.stack)-1]
	}
	defBlock := newBlock(tok, s.current.id, models.BlockTypeDefault)
	parent := s.stack[len(s.stack)-1].block
	defBlock.ParentID = parent.ID
	parent.ChildPtrs = append(parent.ChildPtrs, defBlock)
	s.stack = append(s.stack, stackEntry{indent: tok.Indent + 1, block: defBlock})
}

// LoopKindProperty names the parser-derived loop kind; the underscore marks parser metadata. Exported for analyzer rules.
const LoopKindProperty = "_loopKind"

// Loop kinds. foreach and counted are bounded BY CONSTRUCTION — a FOREACH
// consumes a fixed collection, a counted range has an end; while is
// conditional and may not terminate, and other (dotted Loop.X headers,
// unrecognised) is treated conservatively like while.
const (
	LoopKindForEach = "foreach"
	LoopKindCounted = "counted"
	LoopKindWhile   = "while"
	LoopKindOther   = "other"
)

// classifyLoopKind derives the kind from a loop header line.
func classifyLoopKind(content string) string {
	switch {
	case reLoopForEach.MatchString(content):
		return LoopKindForEach
	case reLoopRange.MatchString(content), reLoopRangeAnon.MatchString(content):
		return LoopKindCounted
	case reLoopWhile.MatchString(content):
		return LoopKindWhile
	default:
		return LoopKindOther
	}
}

func (s *parseState) handleLoopStart(tok Token) {
	if !s.requireSubflow(tok, "loop") || !s.requireDepth(tok) {
		return
	}
	blk := newBlock(tok, s.current.id, models.BlockTypeLoop)
	// Record the loop KIND: distinguishes a loop that can run forever from
	// one bounded by construction — discarded, infinite-loop-risk had to
	// infer it and fired on every FOREACH and counted loop.
	blk.Properties[LoopKindProperty] = classifyLoopKind(tok.Content)
	// Track loop-declared variables so analysis rules can detect scope and usage.
	if m := reLoopForEach.FindStringSubmatch(tok.Content); m != nil {
		// LOOP FOREACH CurrentItem IN List: m[1] = iteration variable (written by the loop), m[3] = bare collection (read).
		blk.Variables = append(blk.Variables, m[1])
		if len(m) >= 4 && m[3] != "" {
			blk.Variables = append(blk.Variables, m[3])
		}
	} else if m := reLoopRange.FindStringSubmatch(tok.Content); m != nil {
		// LOOP LoopIndex FROM x TO y [STEP z] — the named counter variable.
		blk.Variables = append(blk.Variables, m[1])
	}
	s.stack = popStack(s.stack, blk.Indent)
	s.stack = insertIntoTree(s.current, blk, s.stack)
}

func (s *parseState) handleEnd(tok Token) {
	if s.current == nil {
		return
	}
	if s.skipNextEnd {
		s.skipNextEnd = false
		for len(s.stack) > 0 && s.stack[len(s.stack)-1].block.Type != models.BlockTypeErrorHandler {
			s.stack = s.stack[:len(s.stack)-1]
		}
		if len(s.stack) > 0 {
			top := s.stack[len(s.stack)-1].block
			endBlk := &models.Block{
				ID: uuid.NewString(), Name: "End On Block Error", Type: models.BlockTypeEnd,
				RawType: "END", Indent: tok.Indent, LineNumber: tok.Line,
				SubflowID: s.current.id, ParentID: top.ID,
				Properties: map[string]string{"_parentType": string(top.Type)},
				Variables:  []string{},
			}
			endBlk.Tokens = tokenizeBlock(endBlk)
			top.ChildPtrs = append(top.ChildPtrs, endBlk)
			// Deliberately NOT popped: BLOCK + ON BLOCK ERROR is two containers sharing one promoted node, closed
			// by two ENDs — the first closes only the HANDLER section. Popping here put the protected body at ROOT
			// level (HasErrorHandlerAncestor false for every action in a correctly handled block); the BLOCK stays
			// so the second END closes it normally, and a missing second END reports unclosed, not discarded.
		}
		return
	}
	for len(s.stack) > 0 {
		top := s.stack[len(s.stack)-1].block
		if top.Type == models.BlockTypeLoop || top.Type == models.BlockTypeCondition ||
			top.Type == models.BlockTypeErrorHandler || top.Type == models.BlockTypeBlock ||
			top.Type == models.BlockTypeSwitch {

			name := "End"
			switch top.Type {
			case models.BlockTypeLoop:
				name = "End Loop"
			case models.BlockTypeCondition:
				name = "End If"
			case models.BlockTypeSwitch:
				name = "End Switch"
			case models.BlockTypeErrorHandler:
				name = "End On Block Error"
			}

			endBlk := &models.Block{
				ID: uuid.NewString(), Name: name, Type: models.BlockTypeEnd,
				RawType: "END", Indent: tok.Indent, LineNumber: tok.Line,
				SubflowID: s.current.id, ParentID: top.ID,
				Properties: map[string]string{"_parentType": string(top.Type)},
				Variables:  []string{},
			}
			endBlk.Tokens = tokenizeBlock(endBlk)
			top.ChildPtrs = append(top.ChildPtrs, endBlk)
			s.stack = s.stack[:len(s.stack)-1]
			break
		}
		s.stack = s.stack[:len(s.stack)-1]
	}
}

func (s *parseState) handleConditionStart(tok Token) {
	if !s.requireSubflow(tok, "condition") || !s.requireDepth(tok) {
		return
	}
	blk := newBlock(tok, s.current.id, models.BlockTypeCondition)
	s.stack = popStack(s.stack, blk.Indent)
	s.stack = insertIntoTree(s.current, blk, s.stack)
}

func (s *parseState) handleConditionElse(tok Token) {
	if s.current == nil {
		return
	}
	if !s.stackHas(models.BlockTypeCondition) {
		s.parseErrors = append(s.parseErrors, models.ParseError{
			Line: tok.Line, Column: tok.Column, Message: "ELSE outside IF",
			Severity: "warning", Snippet: truncate(tok.Raw, 200),
		})
		return
	}
	for s.stack[len(s.stack)-1].block.Type != models.BlockTypeCondition {
		s.stack = s.stack[:len(s.stack)-1]
	}
	// Built via newBlock so an ELSE IF rung gets the same treatment as any
	// other condition (notably extractVariables); a plain ELSE token already
	// carries Name "Else" / RawType "ELSE", producing the same block the
	// hand-built literal did.
	elseBlock := newBlock(tok, s.current.id, models.BlockTypeElse)
	parent := s.stack[len(s.stack)-1].block
	elseBlock.ParentID = parent.ID
	parent.ChildPtrs = append(parent.ChildPtrs, elseBlock)
	s.stack = append(s.stack, stackEntry{indent: tok.Indent + 1, block: elseBlock})
}

func (s *parseState) handleComment(tok Token) {
	if s.current == nil {
		return
	}
	// Inline error handler: attach retry policy as properties on the parent action block, not a visible child.
	if tok.RawType == "ON_ERROR_INLINE" {
		if len(s.stack) > 0 {
			parent := s.stack[len(s.stack)-1].block
			if m := reOnErrorInlineParams.FindStringSubmatch(tok.Content); m != nil {
				parent.Properties["_retryCount"] = m[1]
				parent.Properties["_retryWait"] = m[2]
				if m[3] != "" {
					parent.Properties["_retryType"] = m[3]
				}
				if m[4] != "" {
					parent.Properties["_retryMinInterval"] = m[4]
				}
				if m[5] != "" {
					parent.Properties["_retryMaxInterval"] = m[5]
				}
			}
		}
		return
	}
	if !s.requireDepth(tok) {
		return
	}
	blk := &models.Block{
		ID: uuid.NewString(), Name: tok.Name, Type: models.BlockTypeComment,
		RawType: "COMMENT", Indent: tok.Indent, LineNumber: tok.Line,
		SubflowID: s.current.id, Properties: map[string]string{}, Variables: []string{},
	}
	blk.Tokens = tokenizeBlock(blk)
	s.stack = popStack(s.stack, blk.Indent)
	s.stack = insertIntoTree(s.current, blk, s.stack)
}

func (s *parseState) handleAction(tok Token) {
	if !s.requireSubflow(tok, "action") || !s.requireDepth(tok) {
		return
	}
	blockType := ClassifyBlockType(tok.RawType)
	blk := newBlock(tok, s.current.id, blockType)
	s.stack = popStack(s.stack, blk.Indent)
	s.stack = insertIntoTree(s.current, blk, s.stack)
}

// lineCount counts lines: strings.Split over-counts by one when text ends with a newline (the common case).
func lineCount(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++ // final line has no trailing newline
	}
	return n
}

// trimPrefixFold removes prefix case-insensitively, matching the classifier's
// (?i) regexes — a case-sensitive trim would leave the keyword in the name.
func trimPrefixFold(s, prefix string) string {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):]
	}
	return s
}

// trimSuffixFold removes suffix from s using case-insensitive comparison.
func trimSuffixFold(s, suffix string) string {
	if len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix) {
		return s[:len(s)-len(suffix)]
	}
	return s
}

func buildDocument(text, fileName string, fileSize int64, subflows []models.Subflow, parseErrors []models.ParseError, totalBlocks, maxDepth int) *models.FlowDocument {
	doc := &models.FlowDocument{
		ID:          uuid.NewString(),
		Name:        strings.TrimSuffix(fileName, filepath.Ext(fileName)),
		FilePath:    "",
		Subflows:    subflows,
		ParseErrors: parseErrors,
		Metadata: models.FlowMetadata{
			BlockCount:   totalBlocks,
			SubflowCount: len(subflows),
			MaxDepth:     maxDepth,
			ParsedAt:     time.Now(),
			FileSize:     fileSize,
			// strings.Split over-counts by one on a trailing newline; see lineCount.
			RawLineCount: lineCount(text),
		},
		BlocksByID:   make(map[string]*models.Block, totalBlocks),
		BlockSubflow: make(map[string]*models.Subflow, totalBlocks),
		SubflowsByID: make(map[string]*models.Subflow, len(subflows)),
	}

	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		// Stamp the source file on subflows without one: SARIF's
		// artifactLocation.uri falls back to SubflowID+".txt" — a UUID the
		// parser MINTS ON EVERY PARSE — so without a stable name GitHub Code
		// Scanning cannot place the alert and nothing correlates between runs.
		if sf.SourceFile == "" {
			sf.SourceFile = fileName
		}
		doc.SubflowsByID[sf.ID] = sf
		for j := range sf.Blocks {
			indexBlockInDoc(doc, sf, &sf.Blocks[j])
		}
	}

	return doc
}

func indexBlockInDoc(doc *models.FlowDocument, sf *models.Subflow, b *models.Block) {
	doc.BlocksByID[b.ID] = b
	doc.BlockSubflow[b.ID] = sf
	for i := range b.Children {
		indexBlockInDoc(doc, sf, &b.Children[i])
	}
}

func newBlock(tok Token, subflowID string, blockType models.BlockType) *models.Block {
	blk := &models.Block{
		ID:            uuid.NewString(),
		Name:          tok.Name,
		Type:          blockType,
		RawType:       tok.RawType,
		Indent:        tok.Indent,
		LineNumber:    tok.Line,
		EndLineNumber: tok.EndLine,
		SubflowID:     subflowID,
		Properties:    parseProperties(tok.Raw),
		Variables:     extractVariables(tok.Raw),
	}

	// For SET x TO expr, inject structured properties parseProperties cannot derive (no colon pairs in SET syntax).
	if tok.SetVarName != "" {
		blk.Properties["_var"] = tok.SetVarName
		blk.Properties["_value"] = tok.SetVarValue
		blk.Properties["_output"] = tok.SetVarName
	}

	// For Variables.* actions, normalise the output variable name.
	normalizeVariableOutput(blk)

	blk.Tokens = tokenizeBlock(blk)
	return blk
}

func tokenizeBlock(blk *models.Block) []models.BlockToken {
	if blk.Type == models.BlockTypeComment {
		return []models.BlockToken{{Type: "text", Value: blk.Name}}
	}

	// GOTO/LABEL — emit a label token so the UI can render the jump target distinctively
	if blk.RawType == "GOTO" || blk.RawType == "LABEL" {
		prefix := "GOTO "
		if blk.RawType == "LABEL" {
			prefix = "LABEL "
		}
		return []models.BlockToken{
			{Type: "text", Value: prefix},
			{Type: "label", Value: blk.Name, Target: blk.Name},
		}
	}

	// Handle CALL actions (Run subflow) - strip "Call " to match stripBlockKeywords
	if blk.RawType == "CALL" || blk.RawType == "DISABLED_CALL" ||
		blk.RawType == "FlowControl.RunSubflow" || blk.RawType == "FlowControl.RunDesktopFlow" {
		target := ""
		switch blk.RawType {
		case "CALL", "DISABLED_CALL":
			if strings.HasPrefix(blk.Name, "Call ") {
				target = strings.TrimPrefix(blk.Name, "Call ")
				target = strings.TrimSuffix(target, " (disabled)")
			}
		case "FlowControl.RunSubflow":
			target = blk.Properties["SubflowName"]
		case "FlowControl.RunDesktopFlow":
			target = blk.Properties["DesktopFlow"]
		}

		if target != "" {
			// We only return the target as a subflow token to match the stripped style
			return []models.BlockToken{
				{Type: "subflow", Value: target, Target: target},
			}
		}
	}

	// Handle SET actions (Variable)
	if blk.Type == models.BlockTypeVariable {
		varName := blk.Properties["_var"]
		if varName == "" {
			varName = blk.Properties["Name"]
		}
		varValue := blk.Properties["_value"]
		if varValue == "" {
			varValue = blk.Properties["Value"]
		}

		if varName != "" {
			tokens := tokenizeVariables("%" + varName + "%")
			tokens = append(tokens, models.BlockToken{Type: "text", Value: " = "})
			if varValue != "" {
				tokens = append(tokens, tokenizeVariables(varValue)...)
			} else {
				tokens = append(tokens, models.BlockToken{Type: "text", Value: "''"})
			}
			return tokens
		}
	}

	// IncreaseVariable → "num += 1"
	if blk.RawType == "Variables.IncreaseVariable" {
		varName := blk.Properties["Value"]
		delta := blk.Properties["IncrementValue"]
		if varName != "" {
			toks := tokenizeVariables("%" + varName + "%")
			toks = append(toks, models.BlockToken{Type: "text", Value: " += "})
			if delta != "" {
				toks = append(toks, tokenizeVariables(delta)...)
			}
			return toks
		}
	}

	// DecreaseVariable → "num -= 34"
	if blk.RawType == "Variables.DecreaseVariable" {
		varName := blk.Properties["Value"]
		delta := blk.Properties["DecrementValue"]
		if varName != "" {
			toks := tokenizeVariables("%" + varName + "%")
			toks = append(toks, models.BlockToken{Type: "text", Value: " -= "})
			if delta != "" {
				toks = append(toks, tokenizeVariables(delta)...)
			}
			return toks
		}
	}

	// AddItemToList → "List ← item"
	if blk.RawType == "Variables.AddItemToList" {
		list := blk.Properties["List"]
		item := blk.Properties["Item"]
		if list != "" {
			toks := tokenizeVariables("%" + list + "%")
			toks = append(toks, models.BlockToken{Type: "text", Value: " ← "})
			if item != "" {
				toks = append(toks, tokenizeVariables(item)...)
			}
			return toks
		}
	}

	// ReverseList → just the list name
	if blk.RawType == "Variables.ReverseList" {
		if list := blk.Properties["List"]; list != "" {
			return tokenizeVariables("%" + list + "%")
		}
	}

	// SortList (SortList and SortListByProperty variants) → just the list name
	if strings.HasPrefix(blk.RawType, "Variables.SortList") {
		if list := blk.Properties["List"]; list != "" {
			return tokenizeVariables("%" + list + "%")
		}
	}

	// CreateNewList → the output list variable name
	if blk.RawType == "Variables.CreateNewList" {
		if out := blk.Properties["_output"]; out != "" {
			return tokenizeVariables("%" + out + "%")
		}
	}

	// ClearList → just the list name
	if blk.RawType == "Variables.ClearList" {
		if list := blk.Properties["List"]; list != "" {
			return tokenizeVariables("%" + list + "%")
		}
	}

	// RemoveItemFromList (by index) → "List[0]"
	if strings.HasPrefix(blk.RawType, "Variables.RemoveItemFromList") {
		list := blk.Properties["List"]
		index := blk.Properties["ItemIndex"]
		if list != "" {
			toks := tokenizeVariables("%" + list + "%")
			toks = append(toks, models.BlockToken{Type: "text", Value: "["})
			if index != "" {
				toks = append(toks, tokenizeVariables(index)...)
			}
			toks = append(toks, models.BlockToken{Type: "text", Value: "]"})
			return toks
		}
	}

	// RemoveDuplicateItemsFromList → just the list name
	if strings.HasPrefix(blk.RawType, "Variables.RemoveDuplicateItemsFromList") {
		if list := blk.Properties["List"]; list != "" {
			return tokenizeVariables("%" + list + "%")
		}
	}

	// Others: strip keywords. For conditions and loops blk.Name already
	// contains the full expression stripBlockKeywords would further trim.
	name := blk.Name
	switch blk.Type {
	case models.BlockTypeCondition:
		name = trimPrefixFold(name, "IF ")
		name = trimSuffixFold(name, " THEN")
	case models.BlockTypeLoop:
		// ForEach: emit variable tokens for both the iteration variable and
		// the collection ("LOOP FOREACH x IN List" → %x% IN %List%) — both
		// clickable in the UI (lineage, usage count).
		if m := reLoopForEach.FindStringSubmatch(name); m != nil {
			iterVar := m[1]
			collection := ""
			if len(m) >= 4 && m[3] != "" {
				collection = m[3]
			}
			toks := tokenizeVariables("%" + iterVar + "%")
			toks = append(toks, models.BlockToken{Type: "text", Value: " IN "})
			if collection != "" {
				toks = append(toks, tokenizeVariables("%"+collection+"%")...)
			}
			return toks
		}

		// Range loop: %LoopIndex% FROM 0 TO 10 STEP 2 — the counter becomes a
		// clickable token; FROM/TO/STEP render as-is (numbers stay text).
		if m := reLoopRange.FindStringSubmatch(name); m != nil {
			varName := m[1]
			from := m[2]
			to := m[3]
			step := ""
			if len(m) >= 5 {
				step = m[4]
			}
			toks := tokenizeVariables("%" + varName + "%")
			toks = append(toks, models.BlockToken{Type: "text", Value: " FROM "})
			toks = append(toks, tokenizeVariables(from)...)
			toks = append(toks, models.BlockToken{Type: "text", Value: " TO "})
			toks = append(toks, tokenizeVariables(to)...)
			if step != "" {
				toks = append(toks, models.BlockToken{Type: "text", Value: " STEP "})
				toks = append(toks, tokenizeVariables(step)...)
			}
			return toks
		}

		name = trimPrefixFold(name, "LOOP FOREACH ")
		name = trimPrefixFold(name, "LOOP WHILE ")
		name = trimPrefixFold(name, "LOOP ")
	case models.BlockTypeSwitch:
		name = trimPrefixFold(name, "SWITCH ")
		// SWITCH arguments are variables/expressions but often lack % signs in the text code
		if !strings.Contains(name, "%") && !strings.Contains(name, "'") && !strings.Contains(name, `"`) {
			name = "%" + name + "%"
		}
	case models.BlockTypeCase:
		name = trimPrefixFold(name, "CASE ")
	}

	return tokenizeVariables(name)
}

func tokenizeVariables(text string) []models.BlockToken {
	if text == "" {
		return nil
	}

	// Find both %variables% and string literals: % can contain strings and
	// strings can contain %. In PAD, %expression% is usually the top level.
	varMatches := reVariableRef.FindAllStringSubmatchIndex(text, -1)
	tokens := make([]models.BlockToken, 0, len(varMatches)*2+1)

	lastPos := 0
	for _, m := range varMatches {
		if m[0] > lastPos {
			tokens = append(tokens, tokenizeStrings(text[lastPos:m[0]])...)
		}

		expression := text[m[2]:m[3]]
		masked := maskStrings(expression)
		target := ""
		// Every index below is read from — and applied back onto — masked,
		// never expression: maskStrings collapses each literal to a single
		// space, so the two strings' lengths diverge and an offset computed
		// against one must never slice the other (see extractVariables).
		ids := reIdentifier.FindAllStringSubmatchIndex(masked, -1)
		for _, idMatch := range ids {
			start, end := idMatch[0], idMatch[1]
			vname := masked[start:end]
			if start > 0 && masked[start-1] == '.' {
				continue
			}
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
			if !isExpressionKeyword(vname) {
				target = vname
				break
			}
		}

		tokens = append(tokens, models.BlockToken{
			Type:   "variable",
			Value:  text[m[0]:m[1]],
			Target: target,
		})

		lastPos = m[1]
	}

	if lastPos < len(text) {
		tokens = append(tokens, tokenizeStrings(text[lastPos:])...)
	}

	return tokens
}

func tokenizeStrings(text string) []models.BlockToken {
	matches := reStringLiteral.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return []models.BlockToken{{Type: "text", Value: text}}
	}

	tokens := make([]models.BlockToken, 0, len(matches)*2+1)
	lastPos := 0
	for _, m := range matches {
		if m[0] > lastPos {
			tokens = append(tokens, models.BlockToken{
				Type:  "text",
				Value: text[lastPos:m[0]],
			})
		}

		val := text[m[0]:m[1]]
		// Format string for UI: $'''abc''' -> "abc"
		displayVal := val
		if strings.HasPrefix(val, "$'''") && strings.HasSuffix(val, "'''") {
			displayVal = "\"" + val[4:len(val)-3] + "\""
		} else if strings.HasPrefix(val, "'''") && strings.HasSuffix(val, "'''") {
			displayVal = "\"" + val[3:len(val)-3] + "\""
		} else if (strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) || (strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"")) {
			displayVal = "\"" + val[1:len(val)-1] + "\""
		}

		tokens = append(tokens, models.BlockToken{
			Type:  "string",
			Value: displayVal,
		})
		lastPos = m[1]
	}

	if lastPos < len(text) {
		tokens = append(tokens, models.BlockToken{
			Type:  "text",
			Value: text[lastPos:],
		})
	}
	return tokens
}

func popStack(stack []stackEntry, indent int) []stackEntry {
	for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
		stack = stack[:len(stack)-1]
	}
	return stack
}

func insertIntoTree(sf *builtSubflow, blk *models.Block, stack []stackEntry) []stackEntry {
	if len(stack) == 0 {
		sf.roots = append(sf.roots, blk)
	} else {
		parent := stack[len(stack)-1].block
		blk.ParentID = parent.ID
		parent.ChildPtrs = append(parent.ChildPtrs, blk)
	}
	return append(stack, stackEntry{indent: blk.Indent, block: blk})
}

func finalizeSubflows(built []*builtSubflow) ([]models.Subflow, int, int) {
	subflows := make([]models.Subflow, 0, len(built))
	totalBlocks := 0
	maxDepth := 0

	for _, bsf := range built {
		sf := models.Subflow{
			ID:        bsf.id,
			Name:      bsf.name,
			Blocks:    []models.Block{},
			Variables: []models.VariableDecl{},
		}
		for _, root := range bsf.roots {
			b := toValueTree(root)
			sf.Blocks = append(sf.Blocks, b)
			totalBlocks++
			totalBlocks += CountDescendants(&b)
			d := computeMaxDepth(&b, 1)
			if d > maxDepth {
				maxDepth = d
			}
		}
		subflows = append(subflows, sf)
	}

	return subflows, totalBlocks, maxDepth
}

func toValueTree(b *models.Block) models.Block {
	out := *b
	out.ChildPtrs = nil
	out.Children = make([]models.Block, len(b.ChildPtrs))
	for i, c := range b.ChildPtrs {
		out.Children[i] = toValueTree(c)
	}
	return out
}

func CountDescendants(b *models.Block) int {
	count := len(b.Children)
	for i := range b.Children {
		count += CountDescendants(&b.Children[i])
	}
	return count
}

func computeMaxDepth(b *models.Block, depth int) int {
	max := depth
	for i := range b.Children {
		d := computeMaxDepth(&b.Children[i], depth+1)
		if d > max {
			max = d
		}
	}
	return max
}

func truncate(s string, maxLen int) string {
	// Byte length upper-bounds rune count, so within-limit-by-bytes cannot exceed it by runes — skips the rune decode.
	if len(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}
