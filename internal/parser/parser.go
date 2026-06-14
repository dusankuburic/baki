package parser

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"pad-analyzer/internal/models"

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

func (s *parseState) processToken(tok Token) {
	switch tok.Kind {
	case TokEmpty:
		return

	case TokError:
		s.parseErrors = append(s.parseErrors, models.ParseError{
			Line: tok.Line, Message: "malformed line",
			Severity: "warning", Snippet: truncate(tok.Raw, 200),
		})
		return

	case TokSubflowStart:
		s.current = &builtSubflow{id: uuid.NewString(), name: tok.Name}
		s.built = append(s.built, s.current)
		s.stack = nil

	case TokSubflowEnd:
		// Check for unclosed blocks before clearing stack
		if len(s.stack) > 0 {
			for _, entry := range s.stack {
				s.parseErrors = append(s.parseErrors, models.ParseError{
					Line:     entry.block.LineNumber,
					Message:  fmt.Sprintf("unclosed block: %s", entry.block.Name),
					Severity: "error",
					Snippet:  entry.block.RawType,
				})
			}
		}
		s.current = nil
		s.stack = nil

	case TokErrorHandlerStart:
		if s.current == nil {
			s.parseErrors = append(s.parseErrors, models.ParseError{
				Line: tok.Line, Message: "error handler outside subflow",
				Severity: "warning", Snippet: truncate(tok.Raw, 200),
			})
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

	case TokBlockStart:
		if s.current == nil {
			s.parseErrors = append(s.parseErrors, models.ParseError{
				Line: tok.Line, Message: "block outside subflow",
				Severity: "warning", Snippet: truncate(tok.Raw, 200),
			})
			return
		}
		if len(s.stack) >= maxNestingDepth {
			s.parseErrors = append(s.parseErrors, models.ParseError{
				Line: tok.Line, Message: fmt.Sprintf("nesting depth exceeds %d", maxNestingDepth),
				Severity: "error", Snippet: truncate(tok.Raw, 200),
			})
			return
		}
		blk := newBlock(tok, s.current.id, models.BlockTypeBlock)
		s.stack = popStack(s.stack, blk.Indent)
		s.stack = insertIntoTree(s.current, blk, s.stack)

	case TokSwitchStart:
		if s.current == nil {
			s.parseErrors = append(s.parseErrors, models.ParseError{
				Line: tok.Line, Message: "switch outside subflow",
				Severity: "warning", Snippet: truncate(tok.Raw, 200),
			})
			return
		}
		if len(s.stack) >= maxNestingDepth {
			s.parseErrors = append(s.parseErrors, models.ParseError{
				Line: tok.Line, Message: fmt.Sprintf("nesting depth exceeds %d", maxNestingDepth),
				Severity: "error", Snippet: truncate(tok.Raw, 200),
			})
			return
		}
		blk := newBlock(tok, s.current.id, models.BlockTypeSwitch)
		s.stack = popStack(s.stack, blk.Indent)
		s.stack = insertIntoTree(s.current, blk, s.stack)

	case TokCase:
		for len(s.stack) > 0 && s.stack[len(s.stack)-1].block.Type != models.BlockTypeSwitch {
			s.stack = s.stack[:len(s.stack)-1]
		}
		if s.current != nil && len(s.stack) > 0 {
			caseBlock := newBlock(tok, s.current.id, models.BlockTypeCase)
			parent := s.stack[len(s.stack)-1].block
			caseBlock.ParentID = parent.ID
			parent.ChildPtrs = append(parent.ChildPtrs, caseBlock)
			s.stack = append(s.stack, stackEntry{indent: tok.Indent + 1, block: caseBlock})
		}

	case TokDefault:
		for len(s.stack) > 0 && s.stack[len(s.stack)-1].block.Type != models.BlockTypeSwitch {
			s.stack = s.stack[:len(s.stack)-1]
		}
		if s.current != nil && len(s.stack) > 0 {
			defBlock := newBlock(tok, s.current.id, models.BlockTypeDefault)
			parent := s.stack[len(s.stack)-1].block
			defBlock.ParentID = parent.ID
			parent.ChildPtrs = append(parent.ChildPtrs, defBlock)
			s.stack = append(s.stack, stackEntry{indent: tok.Indent + 1, block: defBlock})
		}

	case TokLoopStart:
		if s.current == nil {
			s.parseErrors = append(s.parseErrors, models.ParseError{
				Line: tok.Line, Message: "loop outside subflow",
				Severity: "warning", Snippet: truncate(tok.Raw, 200),
			})
			return
		}
		if len(s.stack) >= maxNestingDepth {
			s.parseErrors = append(s.parseErrors, models.ParseError{
				Line: tok.Line, Message: fmt.Sprintf("nesting depth exceeds %d", maxNestingDepth),
				Severity: "error", Snippet: truncate(tok.Raw, 200),
			})
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

	case TokEnd:
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
					Variables: []string{},
				}
				endBlk.Tokens = tokenizeBlock(endBlk)
				top.ChildPtrs = append(top.ChildPtrs, endBlk)
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
					Variables: []string{},
				}
				endBlk.Tokens = tokenizeBlock(endBlk)
				top.ChildPtrs = append(top.ChildPtrs, endBlk)
				s.stack = s.stack[:len(s.stack)-1]
				break
			}
			s.stack = s.stack[:len(s.stack)-1]
		}

	case TokConditionStart:
		if s.current == nil {
			s.parseErrors = append(s.parseErrors, models.ParseError{
				Line: tok.Line, Message: "condition outside subflow",
				Severity: "warning", Snippet: truncate(tok.Raw, 200),
			})
			return
		}
		if len(s.stack) >= maxNestingDepth {
			s.parseErrors = append(s.parseErrors, models.ParseError{
				Line: tok.Line, Message: fmt.Sprintf("nesting depth exceeds %d", maxNestingDepth),
				Severity: "error", Snippet: truncate(tok.Raw, 200),
			})
			return
		}
		blk := newBlock(tok, s.current.id, models.BlockTypeCondition)
		s.stack = popStack(s.stack, blk.Indent)
		s.stack = insertIntoTree(s.current, blk, s.stack)

	case TokConditionElse:
		for len(s.stack) > 0 && s.stack[len(s.stack)-1].block.Type != models.BlockTypeCondition {
			s.stack = s.stack[:len(s.stack)-1]
		}
		if s.current != nil && len(s.stack) > 0 {
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

	case TokComment:
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
		blk := &models.Block{
			ID: uuid.NewString(), Name: tok.Name, Type: models.BlockTypeComment,
			RawType: "COMMENT", Indent: tok.Indent, LineNumber: tok.Line,
			SubflowID: s.current.id, Properties: map[string]string{}, Variables: []string{},
		}
		blk.Tokens = tokenizeBlock(blk)
		s.stack = popStack(s.stack, blk.Indent)
		s.stack = insertIntoTree(s.current, blk, s.stack)

	case TokAction:
		if s.current == nil {
			s.parseErrors = append(s.parseErrors, models.ParseError{
				Line: tok.Line, Message: "action outside subflow",
				Severity: "warning", Snippet: truncate(tok.Raw, 200),
			})
			return
		}
		blockType := ClassifyBlockType(tok.RawType)
		blk := newBlock(tok, s.current.id, blockType)
		s.stack = popStack(s.stack, blk.Indent)
		s.stack = insertIntoTree(s.current, blk, s.stack)
	}
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
			RawLineCount: len(strings.Split(text, "\n")),
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
		ID:         uuid.NewString(),
		Name:       tok.Name,
		Type:       blockType,
		RawType:    tok.RawType,
		Indent:     tok.Indent,
		LineNumber: tok.Line,
		SubflowID:  subflowID,
		Properties: parseProperties(tok.Raw),
		Variables:  extractVariables(tok.Raw),
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
		if blk.RawType == "CALL" || blk.RawType == "DISABLED_CALL" {
			if strings.HasPrefix(blk.Name, "Call ") {
				target = strings.TrimPrefix(blk.Name, "Call ")
				target = strings.TrimSuffix(target, " (disabled)")
			}
		} else if blk.RawType == "FlowControl.RunSubflow" {
			target = blk.Properties["SubflowName"]
		} else if blk.RawType == "FlowControl.RunDesktopFlow" {
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
	if blk.Type == models.BlockTypeCondition {
		name = strings.TrimPrefix(name, "IF ")
		name = strings.TrimPrefix(name, "If ")
		name = strings.TrimSuffix(name, " THEN")
		name = strings.TrimSuffix(name, " Then")
	} else if blk.Type == models.BlockTypeLoop {
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

		name = strings.TrimPrefix(name, "LOOP FOREACH ")
		name = strings.TrimPrefix(name, "LOOP WHILE ")
		name = strings.TrimPrefix(name, "LOOP ")
		name = strings.TrimPrefix(name, "Loop ")
	} else if blk.Type == models.BlockTypeSwitch {
		name = strings.TrimPrefix(name, "SWITCH ")
		name = strings.TrimPrefix(name, "Switch ")
		// SWITCH arguments are variables/expressions but often lack % signs in the text code
		if !strings.Contains(name, "%") && !strings.Contains(name, "'") && !strings.Contains(name, `"`) {
			name = "%" + name + "%"
		}
	} else if blk.Type == models.BlockTypeCase {
		name = strings.TrimPrefix(name, "CASE ")
		name = strings.TrimPrefix(name, "Case ")
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
		ids := reIdentifier.FindAllStringSubmatchIndex(masked, -1)
		for _, idMatch := range ids {
			start, end := idMatch[0], idMatch[1]
			vname := expression[start:end]
			if start > 0 && expression[start-1] == '.' {
				continue
			}
			isFunc := false
			for i := end; i < len(expression); i++ {
				if expression[i] == ' ' || expression[i] == '\t' {
					continue
				}
				if expression[i] == '(' {
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
