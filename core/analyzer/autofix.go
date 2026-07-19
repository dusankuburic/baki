package analyzer

import (
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"

	"pad-core/models"
)

// patchOutOfRangeOps counts patch ops skipped because their target line fell
// outside the source bounds. A fixer emitting an out-of-range line is a bug:
// rather than silently clamping the target (which can splice the edit into the
// wrong place and corrupt the output), ApplyPatch turns the op into a no-op and
// records it here so the fault is observable instead of invisible. Legitimate
// boundary cases — inserting at EOF, a wrap/remove range that spans to the last
// line — are NOT counted.
var patchOutOfRangeOps atomic.Int64

// PatchOutOfRangeOps returns the cumulative number of patch ops skipped for an
// out-of-range target line. A non-zero value indicates a fixer produced an
// invalid line number and its edit was dropped.
func PatchOutOfRangeOps() int64 { return patchOutOfRangeOps.Load() }

func reportOutOfRangePatchOp(kind string, target, nLines int) {
	patchOutOfRangeOps.Add(1)
	slog.Warn("analyzer: patch op target out of range; skipping op (fixer bug)",
		"kind", kind, "target", target, "lines", nLines)
}

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
	indent := strings.Repeat("    ", block.Indent)
	return models.Patch{
		Ops: []models.PatchOp{{
			Kind:       "insert",
			BeforeLine: block.LineNumber,
			Lines:      []string{indent + directive},
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
	// Detect and normalize CRLF: strings.Split(source, "\n") leaves a trailing
	// \r on each line of a CRLF source, and inserted patch lines (built with \n
	// separators) would produce mixed line endings on round-trip — silently
	// corrupting the file's line-ending convention. Normalize to LF internally,
	// then restore the original convention on join.
	crlf := strings.Contains(source, "\r\n")
	raw := source
	if crlf {
		raw = strings.ReplaceAll(source, "\r\n", "\n")
	}
	lines := strings.Split(raw, "\n")

	// Removes first (they delete a range, shifting lines below). Applied
	// bottom-up so an earlier remove doesn't shift a later remove's line
	// targets. Apply-fix patches carry a single op, but sorting keeps a mix
	// sensible. Then wraps (range replace), replaces, appends, inserts.
	removes := make([]models.PatchOp, 0, len(p.Ops))
	for _, op := range p.Ops {
		if op.Kind == "remove" {
			removes = append(removes, op)
		}
	}
	sort.Slice(removes, func(i, j int) bool { return removes[i].StartLine > removes[j].StartLine })
	for _, op := range removes {
		lines = applyRemove(lines, op)
	}

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
		// Valid targets are [1, len+1] — BeforeLine == len+1 appends at EOF.
		// Anything else is a fixer bug: skip rather than clamp into the wrong
		// position.
		if op.BeforeLine < 1 || op.BeforeLine > len(lines)+1 {
			reportOutOfRangePatchOp("insert", op.BeforeLine, len(lines))
			continue
		}
		idx := op.BeforeLine - 1 // insert BEFORE line N ⇒ at slice index N-1
		newLines := make([]string, 0, len(lines)+len(op.Lines))
		newLines = append(newLines, lines[:idx]...)
		newLines = append(newLines, op.Lines...)
		newLines = append(newLines, lines[idx:]...)
		lines = newLines
	}
	out := strings.Join(lines, "\n")
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return out
}

// applyWrap replaces the inclusive 1-based range [StartLine..EndLine] with
// Header, each original line re-indented by IndentDelta 4-space levels, then
// Footer. Header and Footer may contain embedded newlines (multi-line). Used by
// wrap-error-handler (single-line header/footer) and wrap-in-retry (multi-line).
// An out-of-range StartLine is a no-op (fixer bug); EndLine past EOF clamps so a
// range may legitimately span to the last line.
func applyWrap(lines []string, op models.PatchOp) []string {
	if op.StartLine < 1 || op.StartLine > len(lines) {
		reportOutOfRangePatchOp("wrap", op.StartLine, len(lines))
		return lines
	}
	start := op.StartLine - 1
	end := min(max(op.EndLine, start), len(lines))
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

// applyRemove deletes the inclusive 1-based range [StartLine..EndLine],
// returning the shortened slice. An out-of-range StartLine is a no-op (fixer
// bug); EndLine past EOF clamps the same way applyWrap does. Used by
// RemoveBlockPatch to delete a block (and its descendants when EndLine spans
// them).
func applyRemove(lines []string, op models.PatchOp) []string {
	if op.StartLine < 1 || op.StartLine > len(lines) {
		reportOutOfRangePatchOp("remove", op.StartLine, len(lines))
		return lines
	}
	start := op.StartLine - 1
	end := min(max(op.EndLine, start), len(lines))
	out := make([]string, 0, len(lines)-(end-start))
	out = append(out, lines[:start]...)
	out = append(out, lines[end:]...)
	return out
}

// applyReplace does an in-place text substitution within a single line:
// replaces ALL occurrences of Old with New (not just the first). Used to swap a
// hardcoded credential literal for a %Variable% reference — if the same
// secret value appears in multiple properties on the same line, all should be
// replaced.
func applyReplace(lines []string, op models.PatchOp) []string {
	idx := op.StartLine - 1
	if idx < 0 || idx >= len(lines) {
		reportOutOfRangePatchOp("replace", op.StartLine, len(lines))
		return lines
	}
	if op.Old == "" {
		return lines
	}
	lines[idx] = strings.ReplaceAll(lines[idx], op.Old, op.New)
	return lines
}

// applyAppend appends op.Lines[0] to the end of the 1-based line StartLine.
// An out-of-range StartLine is a no-op fault (fixer bug).
func applyAppend(lines []string, op models.PatchOp) []string {
	idx := op.StartLine - 1
	if idx < 0 || idx >= len(lines) {
		reportOutOfRangePatchOp("append", op.StartLine, len(lines))
		return lines
	}
	if len(op.Lines) == 0 {
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
// block's last source line (blockEndLine, not LineNumber) so the property
// appears AFTER any multi-line triple-quoted value closes — appending to the
// first line would inject the text inside the string literal. The appended key
// contains "timeout" so the missing-timeout rule detects it as configured.
func SetTimeoutPatch(block *models.Block) models.Patch {
	return models.Patch{Ops: []models.PatchOp{{
		Kind:      "append",
		StartLine: blockEndLine(block),
		Lines:     []string{" Timeout: " + defaultTimeoutSeconds},
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

// InsertDelayInLoopPatch builds a Patch that inserts a `WAIT 1` action INSIDE
// a loop body (before the first child), resolving slow-pattern. Unlike
// InsertDelayPatch (which goes before the block), this inserts one indent
// level deeper — as the first child of the LOOP — so after re-parse the loop's
// walkBlockTree finds a Wait action and hasWait → true.
func InsertDelayInLoopPatch(block *models.Block) models.Patch {
	if len(block.Children) == 0 {
		return InsertDelayPatch(block)
	}
	firstChildLine := block.Children[0].LineNumber
	if firstChildLine == 0 {
		firstChildLine = block.LineNumber + 1
	}
	indent := strings.Repeat("    ", block.Indent+1)
	delayLine := indent + "WAIT 1"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: firstChildLine,
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
//
//	SET RetryCount TO 0
//	LOOP WHILE %RetryCount% < 3
//	    <action re-indented>
//	    SET RetryCount TO %RetryCount% + 1
//	END
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
// a loop, resolving infinite-loop-risk. The counter initialization goes
// OUTSIDE the loop (before the LOOP line) so it persists across iterations;
// the increment + IF + EXIT go inside the loop body (before the first child).
// After re-parse hasExitCondition detects the "Exit" in EXIT LOOP → true.
//
// Structure produced:
//
//	SET __LoopGuard TO 0      ← outside loop (before LOOP line)
//	LOOP ...
//	    SET __LoopGuard TO %__LoopGuard% + 1
//	    IF %__LoopGuard% > 10000
//	        EXIT LOOP
//	    END
//	END
func InsertExitConditionPatch(block *models.Block) models.Patch {
	if len(block.Children) == 0 {
		return models.Patch{}
	}
	firstChildLine := block.Children[0].LineNumber
	if firstChildLine == 0 {
		firstChildLine = block.LineNumber + 1
	}
	outsideIndent := strings.Repeat("    ", block.Indent)
	indent := strings.Repeat("    ", block.Indent+1)
	inner := strings.Repeat("    ", block.Indent+2)
	insideLines := []string{
		indent + "SET __LoopGuard TO %__LoopGuard% + 1",
		indent + "IF %__LoopGuard% > 10000",
		inner + "EXIT LOOP",
		indent + "END",
	}
	return models.Patch{Ops: []models.PatchOp{{
		// Counter init OUTSIDE the loop so it doesn't reset each iteration
		Kind:       "insert",
		BeforeLine: block.LineNumber,
		Lines:      []string{outsideIndent + "SET __LoopGuard TO 0"},
	}, {
		Kind:       "insert",
		BeforeLine: firstChildLine,
		Lines:      insideLines,
	}}}
}

// RemoveBlockPatch builds a Patch that deletes the block's source lines
// outright (the block itself plus any descendants, via blockEndLine). After
// apply + re-parse the block is gone, so rules that flag a block as redundant /
// dead / unused / disabled / duplicate no longer have a target to fire on.
//
// Used by: duplicate-action, redundant-action, dead-code, dead-data,
// empty-branch, unused-variable, disabled-block. Each of those rules attaches
// its finding to the very block that should be removed, so deleting the
// finding's block resolves it.
//
// For duplicate-action the finding is on the first of a run of ≥3 identical
// actions; removing one leaves N-1 below the minRepeats threshold, so the
// finding is resolved (and since the actions are identical, which one is
// removed is immaterial).
func RemoveBlockPatch(block *models.Block) models.Patch {
	return models.Patch{Ops: []models.PatchOp{{
		Kind:      "remove",
		StartLine: block.LineNumber,
		EndLine:   blockEndLine(block),
	}}}
}

// ParameterizeSqlPatch builds a Patch that rewrites a SQL property value to use
// parameter placeholders instead of interpolated %variables%, resolving
// sql-injection-risk. Each %VarName% in the property value becomes @VarName
// (PAD's parameter syntax); after apply the sql-injection rule's %var% regex
// no longer matches, so the finding is resolved.
//
// Emits one "replace" op per distinct variable reference found (all targeting
// the block's source line). The replace pass applies them in sequence on the
// same line, so multiple vars on one line are all parameterized. Scoped to
// single-line SQL (the property value lives on the block's source line);
// multi-line triple-quoted SQL spanning lines is a known limitation.
func ParameterizeSqlPatch(block *models.Block, propKey string) models.Patch {
	if block.Properties == nil || propKey == "" {
		return models.Patch{}
	}
	sqlText := block.Properties[propKey]
	if sqlText == "" {
		return models.Patch{}
	}
	// Find every %VarName% in the SQL value and emit a replace op turning it
	// into @VarName. De-duplicate so a var used twice yields one op (ReplaceAll
	// handles both occurrences on the line). sqlVarRef has no capture group
	// (it's a match-only regex shared with the rule), so derive the name by
	// trimming the surrounding %.
	seen := make(map[string]bool)
	var ops []models.PatchOp
	for _, m := range sqlVarRef.FindAllString(sqlText, -1) {
		if seen[m] {
			continue
		}
		seen[m] = true
		ops = append(ops, models.PatchOp{
			Kind:      "replace",
			StartLine: block.LineNumber,
			Old:       m,
			New:       "@" + strings.Trim(m, "%"),
		})
	}
	if len(ops) == 0 {
		return models.Patch{}
	}
	return models.Patch{Ops: ops}
}

// AppendOutputPatch builds a Patch that appends ` => %Output_Result%` to the
// CALL block's source line, resolving the uncaptured-output pattern of
// subflow-mismatch. After apply + re-parse, the _output property is set, so
// capturesOutput → true and the finding no longer fires. The output variable
// name is a placeholder the user renames to match the target subflow's actual
// output variable.
func AppendOutputPatch(block *models.Block) models.Patch {
	return models.Patch{Ops: []models.PatchOp{{
		Kind:      "append",
		StartLine: block.LineNumber,
		Lines:     []string{" => %Output_Result%"},
	}}}
}

// InsertDefaultPatch builds a Patch that inserts a DEFAULT branch as the last
// child of a SWITCH block, resolving switch-no-default. After apply + re-parse,
// the SWITCH has a BlockTypeDefault child so the rule no longer fires. The
// default body is a single comment action (the user replaces it with real
// handling). The insert goes AFTER the SWITCH's blockEndLine (before its END).
func InsertDefaultPatch(block *models.Block) models.Patch {
	indent := strings.Repeat("    ", block.Indent+1)
	defaultLine := indent + "DEFAULT"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: blockEndLine(block),
		Lines:      []string{defaultLine},
	}}}
}

// MaskSensitiveVariablePatch builds a Patch that replaces %SensitiveVar% with
// '*** MASKED ***' on the block's source line, resolving sensitive-exposure.
// After apply + re-parse, the variable reference is gone from the block's
// properties, so block.Variables no longer contains the sensitive name and the
// rule doesn't fire. This is a "stop the bleed" fix — the action's behavior
// changes (the masked value replaces the real credential), so the user must
// follow up with proper credential handling.
func MaskSensitiveVariablePatch(block *models.Block, varName string) models.Patch {
	if varName == "" {
		return models.Patch{}
	}
	return models.Patch{Ops: []models.PatchOp{{
		Kind:      "replace",
		StartLine: block.LineNumber,
		Old:       "%" + varName + "%",
		New:       "'*** MASKED ***'",
	}}}
}
