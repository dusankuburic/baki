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

// processToken dispatches one Token to the handler for its Kind. Each handler
// is an independent, self-contained mutation of parseState (no case depends
// on another having run first beyond the shared stack/current fields they all
// read and write directly) — that independence is what makes extracting them
// into named methods pure code movement rather than a behavior change: a
// `return` inside a handler ends that handler exactly where the original
// inline case body would have ended (there is nothing after the switch for
// control to fall into either way).
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

// requireSubflow reports whether processing is currently inside a subflow (has
// seen a TokSubflowStart with no matching TokSubflowEnd yet). If not, it
// records a "<kind> outside subflow" parse error and returns false, so the
// caller can bail out. Shared by every token kind that only makes sense
// inside a subflow.
func (s *parseState) requireSubflow(tok Token, kind string) bool {
	if s.current != nil {
		return true
	}
	s.parseErrors = append(s.parseErrors, models.ParseError{
		Line: tok.Line, Message: kind + " outside subflow",
		Severity: "warning", Snippet: truncate(tok.Raw, 200),
	})
	return false
}

// stackHas reports whether any entry on the stack has the given block type.
// Used by the CASE/DEFAULT/ELSE handlers to detect a misplaced branch before
// popping: previously they popped unconditionally to find their parent type,
// and when it was absent they cleared the entire stack — destroying the
// nesting context and orphaning every subsequent sibling at the root, with no
// parse error recorded.
func (s *parseState) stackHas(t models.BlockType) bool {
	for _, e := range s.stack {
		if e.block.Type == t {
			return true
		}
	}
	return false
}

// requireDepth reports whether the nesting stack has room for one more level.
// If not, it records a "nesting depth exceeds N" parse error and returns
// false. Shared by every block-opening token kind (the flat ones — CASE/
// DEFAULT/ELSE reuse an existing stack level instead of pushing a new nesting
// level, so they don't need this guard).
func (s *parseState) requireDepth(tok Token) bool {
	if len(s.stack) < maxNestingDepth {
		return true
	}
	s.parseErrors = append(s.parseErrors, models.ParseError{
		Line: tok.Line, Message: fmt.Sprintf("nesting depth exceeds %d", maxNestingDepth),
		Severity: "error", Snippet: truncate(tok.Raw, 200),
	})
	return false
}

func (s *parseState) recordMalformedLine(tok Token) {
	s.parseErrors = append(s.parseErrors, models.ParseError{
		Line: tok.Line, Message: "malformed line",
		Severity: "warning", Snippet: truncate(tok.Raw, 200),
	})
}

func (s *parseState) handleSubflowStart(tok Token) {
	// Check for unclosed blocks before starting a new subflow (mirrors
	// handleSubflowEnd). Without this, a missing EndRegion before the next
	// Region silently drops all open IF/LOOP/BLOCK entries.
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

// recordUnclosedBlocks records a parse error for every closable block still
// open on the stack — called before a subflow boundary (start or end) clears
// it, so a missing END isn't silently dropped. Only block types that are
// closed by an END (Loop/Condition/ErrorHandler/Block/Switch) are flagged;
// leaf entries (Action/Comment/Case/Default/Else/End) live on the stack
// between siblings and are popped by popStack, so flagging them produced
// spurious "unclosed block" errors.
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
			Line: tok.Line, Message: "CASE outside SWITCH",
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
			Line: tok.Line, Message: "DEFAULT outside SWITCH",
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

func (s *parseState) handleLoopStart(tok Token) {
	if !s.requireSubflow(tok, "loop") || !s.requireDepth(tok) {
		return
	}
	blk := newBlock(tok, s.current.id, models.BlockTypeLoop)
	// Track loop-declared variables so analysis rules can detect scope and usage.
	if m := reLoopForEach.FindStringSubmatch(tok.Content); m != nil {
		// LOOP FOREACH CurrentItem IN List
		// m[1] = iteration variable ("CurrentItem") — declared/written by the loop.
		// m[3] = bare collection name ("List")      — read/consumed by the loop.
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
			// Pop the ErrorHandler itself, mirroring the normal branch below
			// (line ~299). Without this the ErrorHandler stays on the stack:
			// recordUnclosedBlocks then flags it as "unclosed" and following
			// siblings parent under it until popStack clears them.
			s.stack = s.stack[:len(s.stack)-1]
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
			Line: tok.Line, Message: "ELSE outside IF",
			Severity: "warning", Snippet: truncate(tok.Raw, 200),
		})
		return
	}
	for s.stack[len(s.stack)-1].block.Type != models.BlockTypeCondition {
		s.stack = s.stack[:len(s.stack)-1]
	}
	elseBlock := &models.Block{
		ID: uuid.NewString(), Name: "Else", Type: models.BlockTypeElse,
		RawType: "ELSE", Indent: tok.Indent, LineNumber: tok.Line,
		SubflowID: s.current.id, Properties: map[string]string{}, Variables: []string{},
	}
	elseBlock.Tokens = tokenizeBlock(elseBlock)
	parent := s.stack[len(s.stack)-1].block
	elseBlock.ParentID = parent.ID
	parent.ChildPtrs = append(parent.ChildPtrs, elseBlock)
	s.stack = append(s.stack, stackEntry{indent: tok.Indent + 1, block: elseBlock})
}

func (s *parseState) handleComment(tok Token) {
	if s.current == nil {
		return
	}
	// Inline error handler: attach retry policy as properties on the parent action block
	// rather than creating a visible child comment block.
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

// lineCount returns the number of lines in text. strings.Split(text, "\n")
// over-counts by one when text ends with a newline (it produces a trailing
// empty element), which is the overwhelmingly common case for source files.
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

// trimPrefixFold removes prefix from s using case-insensitive comparison.
// The classifier regexes are case-insensitive ((?i)), so a lowercase/mixed-case
// keyword like "switch %x%" classifies as a SWITCH but the display-name
// stripper used case-sensitive TrimPrefix — leaving the keyword in the rendered
// name. This matches the classifier's behavior.
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
			// strings.Split(text, "\n") over-counts by one when the text ends
			// with a newline (the common case): it produces a trailing empty
			// element. Count newlines and add one only if the final line lacks
			// a trailing newline.
			RawLineCount: lineCount(text),
		},
		BlocksByID:   make(map[string]*models.Block, totalBlocks),
		BlockSubflow: make(map[string]*models.Subflow, totalBlocks),
		SubflowsByID: make(map[string]*models.Subflow, len(subflows)),
	}

	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
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

	// For SET x TO expr, inject structured properties that parseProperties cannot
	// derive (no colon-delimited pairs in SET syntax).
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

	// ReverseList → just the list name as a variable reference
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

	// Handle other types by stripping keywords
	name := blk.Name
	// For conditions and loops, blk.Name already contains the full expression
	// which stripBlockKeywords would further trim.
	switch blk.Type {
	case models.BlockTypeCondition:
		name = trimPrefixFold(name, "IF ")
		name = trimSuffixFold(name, " THEN")
	case models.BlockTypeLoop:
		// ForEach: emit variable-styled tokens for both the iteration variable and
		// the collection — "LOOP FOREACH CurrentItem IN List" → %CurrentItem% IN %List%.
		// Both become clickable variable tokens in the UI (lineage, usage count).
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

		// Range loop: "LOOP LoopIndex FROM 0 TO 10 STEP 2"
		// → %LoopIndex% FROM 0 TO 10 STEP 2
		// The counter variable becomes a clickable variable token; FROM/TO/STEP
		// values are rendered as-is (plain numbers stay text, %vars% become tokens).
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

	// We need to find both %variables% and string literals.
	// Since % can contain strings and strings can contain %, we need to be careful.
	// In PAD, %expression% is usually the top level.

	varMatches := reVariableRef.FindAllStringSubmatchIndex(text, -1)
	tokens := make([]models.BlockToken, 0, len(varMatches)*2+1)

	lastPos := 0
	for _, m := range varMatches {
		if m[0] > lastPos {
			// Search for strings in the text between variables
			tokens = append(tokens, tokenizeStrings(text[lastPos:m[0]])...)
		}

		expression := text[m[2]:m[3]]
		masked := maskStrings(expression)
		target := ""
		// Every index below is read from — and applied back onto — masked, never
		// expression: maskStrings round-trips through []rune, so on malformed
		// UTF-8 masked's byte length can diverge from expression's, and slicing
		// expression with a masked-derived offset then panics ("slice bounds out
		// of range"). See the identical comment on extractVariables in
		// extractors.go for the full explanation.
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
			if !reExpressionKeyword.MatchString(vname) {
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
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}
