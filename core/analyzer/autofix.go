package analyzer

import (
	"sort"
	"strings"

	"pad-core/models"
)

// SuppressFindingPatch builds a Patch that inserts a `# pad-ignore[ruleID]`
// comment immediately before the block's source line, silencing the finding at
// the source (honored by the analyzer, the CLI gate, baselines, and CI — not
// just a UI overlay). ruleID "" suppresses all rules on that block.
//
// This is the safest first auto-fix: a one-line insert into the raw source,
// no structural change, and the analyzer already honors the directive, so a
// re-analysis after apply is guaranteed to drop the finding.
func SuppressFindingPatch(block *models.Block, ruleID string) models.Patch {
	directive := "# pad-ignore"
	if ruleID != "" {
		directive = "# pad-ignore[" + ruleID + "]"
	}
	return models.Patch{
		Ops: []models.PatchOp{{
			Kind:       "insert",
			BeforeLine: block.LineNumber,
			Lines:      []string{directive},
		}},
	}
}

// ApplyPatch applies a Patch's line-level edits to raw source text and returns
// the patched text. Insert ops are applied BOTTOM-UP (highest BeforeLine first)
// so an insert doesn't shift the line targets of later ops. A wrap op replaces
// its inclusive range with [Header, re-indented range, Footer]. It is purely
// textual — the caller re-parses the result to validate structure.
func ApplyPatch(source string, p models.Patch) string {
	if len(p.Ops) == 0 {
		return source
	}
	lines := strings.Split(source, "\n")

	// Wraps first (they replace a range), then replaces, then appends, then
	// inserts bottom-up. Apply-fix patches carry a single op, so multi-op
	// interaction is minimal; this ordering keeps a mix sensible.
	for _, op := range p.Ops {
		if op.Kind != "wrap" {
			continue
		}
		lines = applyWrap(lines, op)
	}

	for _, op := range p.Ops {
		if op.Kind != "replace" {
			continue
		}
		lines = applyReplace(lines, op)
	}

	for _, op := range p.Ops {
		if op.Kind != "append" {
			continue
		}
		lines = applyAppend(lines, op)
	}

	inserts := make([]models.PatchOp, 0, len(p.Ops))
	for _, op := range p.Ops {
		if op.Kind == "insert" {
			inserts = append(inserts, op)
		}
	}
	sort.Slice(inserts, func(i, j int) bool { return inserts[i].BeforeLine > inserts[j].BeforeLine })
	for _, op := range inserts {
		idx := op.BeforeLine - 1 // insert BEFORE line N ⇒ at slice index N-1
		if idx < 0 {
			idx = 0
		}
		if idx > len(lines) {
			idx = len(lines)
		}
		newLines := make([]string, 0, len(lines)+len(op.Lines))
		newLines = append(newLines, lines[:idx]...)
		newLines = append(newLines, op.Lines...)
		newLines = append(newLines, lines[idx:]...)
		lines = newLines
	}
	return strings.Join(lines, "\n")
}

// applyWrap replaces the inclusive 1-based range [StartLine..EndLine] with
// Header, each original line re-indented by IndentDelta 4-space levels, then
// Footer. Header and Footer may contain embedded newlines (multi-line). Used by
// wrap-error-handler (single-line header/footer) and wrap-in-retry (multi-line).
// Out-of-range bounds are clamped.
func applyWrap(lines []string, op models.PatchOp) []string {
	start := op.StartLine - 1
	end := op.EndLine
	if start < 0 {
		start = 0
	}
	if start > len(lines) {
		start = len(lines)
	}
	if end < start {
		end = start
	}
	if end > len(lines) {
		end = len(lines)
	}
	pad := ""
	if op.IndentDelta > 0 {
		pad = strings.Repeat("    ", op.IndentDelta)
	}
	wrapped := lines[start:end]
	reindented := make([]string, 0, len(wrapped)+4)
	if op.Header != "" {
		reindented = append(reindented, strings.Split(op.Header, "\n")...)
	}
	for _, l := range wrapped {
		reindented = append(reindented, pad+l)
	}
	if op.Footer != "" {
		reindented = append(reindented, strings.Split(op.Footer, "\n")...)
	}
	out := make([]string, 0, len(lines)-len(wrapped)+len(reindented))
	out = append(out, lines[:start]...)
	out = append(out, reindented...)
	out = append(out, lines[end:]...)
	return out
}

// applyReplace does an in-place text substitution within a single line:
// the first occurrence of Old in line StartLine (1-based) is replaced with New.
// Used to swap a hardcoded credential literal for a %Variable% reference.
func applyReplace(lines []string, op models.PatchOp) []string {
	idx := op.StartLine - 1
	if idx < 0 || idx >= len(lines) || op.Old == "" {
		return lines
	}
	lines[idx] = strings.Replace(lines[idx], op.Old, op.New, 1)
	return lines
}

// applyAppend appends op.Lines[0] to the end of the 1-based line StartLine.
// Out-of-range bounds are clamped (no-op if the line doesn't exist).
func applyAppend(lines []string, op models.PatchOp) []string {
	idx := op.StartLine - 1
	if idx < 0 || idx >= len(lines) || len(op.Lines) == 0 {
		return lines
	}
	lines[idx] += op.Lines[0]
	return lines
}

// blockEndLine returns the last line number occupied by a block (itself plus
// any descendants). A leaf action occupies just its own LineNumber; a compound
// block (IF/LOOP/error handler) spans to its last child's line. Used by
// WrapInErrorHandlerPatch to size the wrapped range.
func blockEndLine(block *models.Block) int {
	end := block.LineNumber
	for i := range block.Children {
		if e := blockEndLine(&block.Children[i]); e > end {
			end = e
		}
	}
	return end
}

// WrapInErrorHandlerPatch builds a Patch that wraps the block's source lines in
// an `ON BLOCK ERROR … END` block, re-indenting the wrapped lines one level so
// they become the handler's children. After apply + re-parse the block has an
// error-handler ancestor, so the unhandled-error / file-op-no-error-handler
// findings no longer fire on it.
//
// The handler body is intentionally minimal (the wrapped action itself); the
// user can extend it in PAD. The wrap targets a LEAF action by default — for a
// compound block the whole span is wrapped.
func WrapInErrorHandlerPatch(block *models.Block) models.Patch {
	indent := strings.Repeat("    ", block.Indent)
	return models.Patch{Ops: []models.PatchOp{{
		Kind:        "wrap",
		StartLine:   block.LineNumber,
		EndLine:     blockEndLine(block),
		Header:      indent + "ON BLOCK ERROR",
		Footer:      indent + "END",
		IndentDelta: 1,
	}}}
}

// InsertClosePatch builds a Patch that inserts a matching Close action after
// the block that opened a resource (resolves resource-leak). The close action
// references the open's output variable so the resource-leak rule detects it as
// closed. The property name ("Handle") is a generic placeholder — the user may
// need to adjust it to PAD's actual property name (Document/Connection/etc.)
// when importing back, but the finding is resolved structurally.
func InsertClosePatch(block *models.Block) models.Patch {
	var closePrefix string
	for _, p := range resourcePairs {
		if strings.HasPrefix(block.RawType, p.openPrefix) {
			closePrefix = p.closePrefix
			break
		}
	}
	if closePrefix == "" {
		return models.Patch{}
	}
	outputVar := block.Properties["_output"]
	if outputVar == "" {
		outputVar = block.Properties["output"]
	}
	if outputVar == "" {
		return models.Patch{}
	}
	indent := strings.Repeat("    ", block.Indent)
	closeLine := indent + closePrefix + " Handle: %" + outputVar + "%"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: blockEndLine(block) + 1,
		Lines:      []string{closeLine},
	}}}
}

// defaultTimeoutSeconds is the value SetTimeoutPatch stamps onto actions that
// lack an explicit timeout. 30s is a conservative default that prevents
// indefinite hangs while allowing slow-but-legitimate responses.
const defaultTimeoutSeconds = "30"

// SetTimeoutPatch builds a Patch that appends a ` Timeout: <n>` property to the
// block's source line, resolving missing-timeout. The original line content is
// preserved (the property is appended), so parameter order, quoting, and
// comments survive. The appended key contains "timeout" so the missing-timeout
// rule detects it as configured on re-analysis.
func SetTimeoutPatch(block *models.Block) models.Patch {
	return models.Patch{Ops: []models.PatchOp{{
		Kind:     "append",
		StartLine: block.LineNumber,
		Lines:    []string{" Timeout: " + defaultTimeoutSeconds},
	}}}
}

// InsertDelayPatch builds a Patch that inserts a `WAIT 1` action before the
// block, resolving missing-delay. After re-parse the previous sibling is a
// WAIT action (isWaitAction → true) so the rule no longer fires on the current
// block. The delay value (1s) is a conservative default for UI automation.
func InsertDelayPatch(block *models.Block) models.Patch {
	indent := strings.Repeat("    ", block.Indent)
	delayLine := indent + "WAIT 1"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: block.LineNumber,
		Lines:      []string{delayLine},
	}}}
}

// InsertHandlerLogPatch builds a Patch that inserts a logging action before the
// error handler's END line, resolving empty-handler. After re-parse the handler
// has a real child (non-END) so hasRealChildren → true. The inserted action
// (ShowMessageBox) is a placeholder the user replaces with their own error
// handling logic.
func InsertHandlerLogPatch(block *models.Block) models.Patch {
	indent := strings.Repeat("    ", block.Indent+1)
	logLine := indent + "Display.ShowMessageBox Message: 'Error occurred'"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: blockEndLine(block),
		Lines:      []string{logLine},
	}}}
}

// InsertVariableInitPatch builds a Patch that inserts a `SET <var> TO ""` action
// before the block that reads the variable, resolving uninitialized-variable.
// After re-parse the SET action's _output property registers the variable in
// WritersByVar, so isAssignedAnywhere → true. The default value (empty string)
// is a safe placeholder the user replaces with the actual initial value.
func InsertVariableInitPatch(block *models.Block, varName string) models.Patch {
	if varName == "" {
		return models.Patch{}
	}
	indent := strings.Repeat("    ", block.Indent)
	setLine := indent + "SET " + varName + " TO \"\""
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: block.LineNumber,
		Lines:      []string{setLine},
	}}}
}

// InsertErrorLogPatch builds a Patch that inserts an error-logging action before
// the error handler's END line, resolving error-swallow. Unlike the empty-
// handler fixer, this one references %LastError% (a PAD system variable) so the
// inserted action is genuinely useful — it surfaces the actual error message.
// After re-parse handlerDoesSomething detects the "error" reference → true.
func InsertErrorLogPatch(block *models.Block) models.Patch {
	indent := strings.Repeat("    ", block.Indent+1)
	logLine := indent + "Display.ShowMessageBox Message: %LastError%"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: blockEndLine(block),
		Lines:      []string{logLine},
	}}}
}

// ReplaceWithVariablePatch builds a Patch that replaces a hardcoded credential
// literal in a property value with a %input_<key>% variable reference. The
// variable uses the `input_` prefix convention so the uninitialized-variable
// rule treats it as an externally-provided value (the user declares it as a
// sensitive flow input or vault lookup). Single replace op — no SET is inserted
// (a SET would just move the secret to another location the rule still flags).
func ReplaceWithVariablePatch(block *models.Block, propKey string) models.Patch {
	if block.Properties == nil {
		return models.Patch{}
	}
	propValue := block.Properties[propKey]
	if propValue == "" {
		return models.Patch{}
	}
	varName := "input_" + strings.ToLower(propKey)
	return models.Patch{Ops: []models.PatchOp{{
		Kind:      "replace",
		StartLine: block.LineNumber,
		Old:       propValue,
		New:       "%" + varName + "%",
	}}}
}

// WrapInRetryPatch builds a Patch that wraps the block in a retry loop with a
// counter, resolving missing-retry. The loop name contains "RetryCount" so the
// missing-retry rule's isInsideRetryLoop heuristic detects it as a retry loop.
// Uses a multi-line header (SET counter + LOOP) and multi-line footer (counter
// increment + END) via the wrap op's newline-splitting support.
//
// Structure produced:
//   SET RetryCount TO 0
//   LOOP WHILE %RetryCount% < 3
//       <action re-indented>
//       SET RetryCount TO %RetryCount% + 1
//   END
func WrapInRetryPatch(block *models.Block) models.Patch {
	indent := strings.Repeat("    ", block.Indent)
	inner := strings.Repeat("    ", block.Indent+1)
	header := indent + "SET RetryCount TO 0\n" + indent + "LOOP WHILE %RetryCount% < 3"
	footer := inner + "SET RetryCount TO %RetryCount% + 1\n" + indent + "END"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:        "wrap",
		StartLine:   block.LineNumber,
		EndLine:     blockEndLine(block),
		Header:      header,
		Footer:      footer,
		IndentDelta: 1,
	}}}
}

// InsertExitConditionPatch builds a Patch that inserts an exit condition inside
// a loop, resolving infinite-loop-risk. Inserts a counter SET before the loop
// body and an IF + EXIT_LOOP inside the loop (before the first child). After
// re-parse hasExitCondition detects the "Exit" in EXIT_LOOP → true.
//
// Structure produced (inserted before the loop's first child):
//   SET __LoopGuard TO 0
//   SET __LoopGuard TO %__LoopGuard% + 1
//   IF %__LoopGuard% > 10000
//       EXIT_LOOP
//   END
func InsertExitConditionPatch(block *models.Block) models.Patch {
	if len(block.Children) == 0 {
		return models.Patch{}
	}
	firstChildLine := block.Children[0].LineNumber
	if firstChildLine == 0 {
		firstChildLine = block.LineNumber + 1
	}
	indent := strings.Repeat("    ", block.Indent+1)
	inner := strings.Repeat("    ", block.Indent+2)
	lines := []string{
		indent + "SET __LoopGuard TO 0",
		indent + "SET __LoopGuard TO %__LoopGuard% + 1",
		indent + "IF %__LoopGuard% > 10000",
		inner + "EXIT LOOP",
		indent + "END",
	}
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: firstChildLine,
		Lines:      lines,
	}}}
}
