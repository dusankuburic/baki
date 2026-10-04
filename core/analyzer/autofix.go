package analyzer

import (
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"

	"pad-core/models"
)

var patchOutOfRangeOps atomic.Int64

// PatchOutOfRangeOps returns the cumulative count of patch ops skipped for an out-of-range target line (non-zero = fixer bug).
func PatchOutOfRangeOps() int64 { return patchOutOfRangeOps.Load() }

func reportOutOfRangePatchOp(kind string, target, nLines int) {
	patchOutOfRangeOps.Add(1)
	slog.Warn("analyzer: patch op target out of range; skipping op (fixer bug)",
		"kind", kind, "target", target, "lines", nLines)
}

// indentCols: block.Indent is the RAW leading column count (tab = 4), NOT 4-space
// levels; scaling it once livelocked the fix loop (TestFixerIndentation_NestsCorrectly).
func indentCols(block *models.Block) string { return strings.Repeat(" ", block.Indent) }

// indentColsDeeper is indentCols shifted n levels (4 columns per level); the
// parser nests by strictly-greater column, so +4 always lands inside.
func indentColsDeeper(block *models.Block, levels int) string {
	return strings.Repeat(" ", block.Indent+4*levels)
}

// SuppressFindingPatch inserts `# pad-ignore[ruleID]` before the block's line; ruleID "" suppresses all rules.
func SuppressFindingPatch(block *models.Block, ruleID string) models.Patch {
	directive := "# pad-ignore"
	if ruleID != "" {
		directive = "# pad-ignore[" + ruleID + "]"
	}
	indent := indentCols(block)
	return models.Patch{
		Ops: []models.PatchOp{{
			Kind:       "insert",
			BeforeLine: block.LineNumber,
			Lines:      []string{indent + directive},
		}},
	}
}

// ApplyPatch applies a Patch's line-level edits to raw source text; purely
// textual — the caller re-parses the result to validate structure.
func ApplyPatch(source string, p models.Patch) string {
	if len(p.Ops) == 0 {
		return source
	}
	// Normalize CRLF: \n-built patch lines would corrupt a CRLF source on
	// round-trip; restore the original convention on join.
	crlf := strings.Contains(source, "\r\n")
	raw := source
	if crlf {
		raw = strings.ReplaceAll(source, "\r\n", "\n")
	}
	lines := strings.Split(raw, "\n")

	// Removes first (bottom-up so an earlier remove doesn't shift a later
	// one's targets), then wraps, replaces, appends, inserts (bottom-up).
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
		if op.BeforeLine < 1 || op.BeforeLine > len(lines)+1 {
			reportOutOfRangePatchOp("insert", op.BeforeLine, len(lines))
			continue
		}
		idx := op.BeforeLine - 1
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

// applyWrap replaces [StartLine..EndLine] with Header, re-indented lines, then
// Footer; out-of-range StartLine is a no-op (fixer bug), EndLine past EOF clamps.
func applyWrap(lines []string, op models.PatchOp) []string {
	if op.StartLine < 1 || op.StartLine > len(lines) {
		reportOutOfRangePatchOp("wrap", op.StartLine, len(lines))
		return lines
	}
	start := op.StartLine - 1
	// EndLine doubles as the 0-based exclusive bound; guard inverted ranges and clamp to EOF.
	end := op.EndLine
	if end < start {
		end = start
	}
	end = min(end, len(lines))
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

// applyRemove deletes [StartLine..EndLine]; out-of-range StartLine is a no-op
// fault, EndLine past EOF clamps (RemoveBlockPatch spans descendants).
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

// applyReplace replaces ALL occurrences of Old with New on one line (the same
// secret on several properties should all go); multi-line Old (a triple-quoted
// value) falls back to its first line.
func applyReplace(lines []string, op models.PatchOp) []string {
	idx := op.StartLine - 1
	if idx < 0 || idx >= len(lines) {
		reportOutOfRangePatchOp("replace", op.StartLine, len(lines))
		return lines
	}
	if op.Old == "" {
		return lines
	}
	old := op.Old
	if nl := strings.IndexByte(old, '\n'); nl >= 0 {
		old = old[:nl]
		if old == "" {
			return lines
		}
	}
	if op.Property != "" {
		lines[idx] = replaceInProperty(lines[idx], op.Property, old, op.New)
		return lines
	}
	lines[idx] = strings.ReplaceAll(lines[idx], old, op.New)
	return lines
}

// replaceInProperty replaces the FIRST occurrence of old after `property:`;
// the key must start at a word boundary, else the line is returned unchanged.
func replaceInProperty(line, property, old, replacement string) string {
	keyAt := -1
	for i := 0; ; {
		j := strings.Index(line[i:], property+":")
		if j < 0 {
			break
		}
		at := i + j
		if at == 0 || line[at-1] == ' ' || line[at-1] == '\t' {
			keyAt = at
			break
		}
		i = at + 1
	}
	if keyAt < 0 {
		return line
	}
	valueFrom := keyAt + len(property) + 1
	rel := strings.Index(line[valueFrom:], old)
	if rel < 0 {
		return line
	}
	at := valueFrom + rel
	return line[:at] + replacement + line[at+len(old):]
}

// applyAppend appends op.Lines[0] to line StartLine; out-of-range StartLine is a no-op fault (fixer bug).
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

// BlockSpan returns the block's inclusive source-line range (multi-line
// literal tail and all descendants). Inline RETRY directives carry no line —
// callers must extend `end` themselves.
func BlockSpan(block *models.Block) (start, end int) {
	return block.LineNumber, blockEndLine(block)
}

// blockEndLine: EndLineNumber (triple-quoted tail) or last descendant's line;
// fixers place patches after this.
func blockEndLine(block *models.Block) int {
	end := block.LineNumber
	if block.EndLineNumber > end {
		end = block.EndLineNumber
	}
	for i := range block.Children {
		if e := blockEndLine(&block.Children[i]); e > end {
			end = e
		}
	}
	return end
}

// WrapInErrorHandlerPatch wraps the block in `ON BLOCK ERROR … END`, re-indenting one level.
func WrapInErrorHandlerPatch(block *models.Block) models.Patch {
	indent := indentCols(block)
	return models.Patch{Ops: []models.PatchOp{{
		Kind:        "wrap",
		StartLine:   block.LineNumber,
		EndLine:     blockEndLine(block),
		Header:      indent + "ON BLOCK ERROR",
		Footer:      indent + "END",
		IndentDelta: 1,
	}}}
}

// InsertClosePatch inserts a matching Close after the resource open, referencing
// the open's output variable so resource-leak sees it closed.
func InsertClosePatch(block *models.Block) models.Patch {
	pair := matchResourcePair(block.RawType)
	if pair == nil {
		return models.Patch{}
	}
	closePrefix := pair.closeActionName()
	outputVar := block.Properties["_output"]
	if outputVar == "" {
		outputVar = block.Properties["output"]
	}
	if outputVar == "" {
		return models.Patch{}
	}
	indent := indentCols(block)
	closeLine := indent + closePrefix + " Handle: %" + outputVar + "%"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: blockEndLine(block) + 1,
		Lines:      []string{closeLine},
	}}}
}

// defaultTimeoutSeconds: 30s prevents indefinite hangs while allowing slow legitimate responses.
const defaultTimeoutSeconds = "30"

// SetTimeoutPatch appends ` Timeout: <n>` to the block's LAST line (appending to
// the first would inject into a triple-quoted value); the key contains "timeout"
// so missing-timeout detects it.
func SetTimeoutPatch(block *models.Block) models.Patch {
	return models.Patch{Ops: []models.PatchOp{{
		Kind:      "append",
		StartLine: blockEndLine(block),
		Lines:     []string{" Timeout: " + defaultTimeoutSeconds},
	}}}
}

// InsertDelayPatch inserts `WAIT 1` before the block, resolving missing-delay.
func InsertDelayPatch(block *models.Block) models.Patch {
	indent := indentCols(block)
	delayLine := indent + "WAIT 1"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: block.LineNumber,
		Lines:      []string{delayLine},
	}}}
}

// InsertDelayInLoopPatch inserts `WAIT 1` inside the loop body, one indent deeper than InsertDelayPatch.
func InsertDelayInLoopPatch(block *models.Block) models.Patch {
	if len(block.Children) == 0 {
		return InsertDelayPatch(block)
	}
	firstChildLine := block.Children[0].LineNumber
	if firstChildLine == 0 {
		firstChildLine = block.LineNumber + 1
	}
	indent := indentColsDeeper(block, 1)
	delayLine := indent + "WAIT 1"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: firstChildLine,
		Lines:      []string{delayLine},
	}}}
}

// InsertHandlerLogPatch inserts a placeholder logging action before the handler's END.
func InsertHandlerLogPatch(block *models.Block) models.Patch {
	indent := indentColsDeeper(block, 1)
	logLine := indent + "Display.ShowMessageBox Message: 'Error occurred'"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: blockEndLine(block),
		Lines:      []string{logLine},
	}}}
}

// InsertVariableInitPatch inserts `SET <var> TO ""` before the first read; the
// SET's _output registers it in WritersByVar.
func InsertVariableInitPatch(block *models.Block, varName string) models.Patch {
	if varName == "" {
		return models.Patch{}
	}
	indent := indentCols(block)
	setLine := indent + "SET " + varName + " TO \"\""
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: block.LineNumber,
		Lines:      []string{setLine},
	}}}
}

// InsertErrorLogPatch inserts an error-logging action before the handler's END; %LastError% carries the actual message.
func InsertErrorLogPatch(block *models.Block) models.Patch {
	indent := indentColsDeeper(block, 1)
	logLine := indent + "Display.ShowMessageBox Message: %LastError%"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: blockEndLine(block),
		Lines:      []string{logLine},
	}}}
}

// ReplaceWithVariablePatch replaces a hardcoded literal with %input_<key>%;
// a SET would just move the secret somewhere the rule still flags.
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
		Property:  propKey,
		Old:       propValue,
		New:       "%" + varName + "%",
	}}}
}

// WrapInRetryPatch appends PAD's native inline retry (`ON ERROR REPEAT 3
// TIMES WAIT 5`). NOT a counting loop: no error check or success exit, and it
// would re-run side effects; the parser folds the directive into
// _retryCount/_retryWait, standing missing-retry and unhandled-error down.
func WrapInRetryPatch(block *models.Block) models.Patch {
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: blockEndLine(block) + 1,
		Lines:      []string{indentCols(block) + "ON ERROR REPEAT 3 TIMES WAIT 5"},
	}}}
}

// InsertExitConditionPatch inserts a loop guard: the counter init goes OUTSIDE
// the loop so it persists across iterations, the increment + IF + EXIT LOOP
// before the first child (hasExitCondition detects the "Exit").
func InsertExitConditionPatch(block *models.Block) models.Patch {
	if len(block.Children) == 0 {
		return models.Patch{}
	}
	firstChildLine := block.Children[0].LineNumber
	if firstChildLine == 0 {
		firstChildLine = block.LineNumber + 1
	}
	outsideIndent := indentCols(block)
	indent := indentColsDeeper(block, 1)
	inner := indentColsDeeper(block, 2)
	insideLines := []string{
		indent + "SET __LoopGuard TO %__LoopGuard% + 1",
		indent + "IF %__LoopGuard% > 10000",
		inner + "EXIT LOOP",
		indent + "END",
	}
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: block.LineNumber,
		Lines:      []string{outsideIndent + "SET __LoopGuard TO 0"},
	}, {
		Kind:       "insert",
		BeforeLine: firstChildLine,
		Lines:      insideLines,
	}}}
}

// RemoveBlockPatch deletes the block's lines outright (itself plus descendants);
// for duplicate-action the finding anchors the first of a ≥3 identical run.
func RemoveBlockPatch(block *models.Block) models.Patch {
	return models.Patch{Ops: []models.PatchOp{{
		Kind:      "remove",
		StartLine: block.LineNumber,
		EndLine:   blockEndLine(block),
	}}}
}

// ParameterizeSqlPatch rewrites each %VarName% in a SQL property as @VarName
// (PAD parameter syntax), one op per distinct var; scoped to single-line SQL
// (multi-line triple-quoted SQL is a known limitation).
func ParameterizeSqlPatch(block *models.Block, propKey string) models.Patch {
	if block.Properties == nil || propKey == "" {
		return models.Patch{}
	}
	sqlText := block.Properties[propKey]
	if sqlText == "" {
		return models.Patch{}
	}
	// De-duplicate: one op per var (ReplaceAll handles repeated occurrences).
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

// AppendOutputPatch appends ` => Output_Result` to the CALL line. The name must
// be BARE: the parser's reOutputVar only matches `=>\s*(\w+)\s*$`, so a
// %wrapped% name would leave _output unset and the finding would survive.
func AppendOutputPatch(block *models.Block) models.Patch {
	return models.Patch{Ops: []models.PatchOp{{
		Kind:      "append",
		StartLine: block.LineNumber,
		Lines:     []string{" => Output_Result"},
	}}}
}

// InsertDefaultPatch inserts a DEFAULT branch as the SWITCH's last child, resolving switch-no-default.
func InsertDefaultPatch(block *models.Block) models.Patch {
	indent := indentColsDeeper(block, 1)
	defaultLine := indent + "DEFAULT"
	return models.Patch{Ops: []models.PatchOp{{
		Kind:       "insert",
		BeforeLine: blockEndLine(block),
		Lines:      []string{defaultLine},
	}}}
}

// MaskSensitiveVariablePatch replaces %var% with '*** MASKED ***' on the block's
// line — a stop-the-bleed fix; behavior changes, so the user must follow up.
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

// UpgradeToHttpsPatch replaces a cleartext http:// prefix with https:// on the block's line.
func UpgradeToHttpsPatch(block *models.Block) models.Patch {
	return models.Patch{Ops: []models.PatchOp{{
		Kind:      "replace",
		StartLine: block.LineNumber,
		Old:       "http://",
		New:       "https://",
	}}}
}

// SanitizeCommandVarsPatch strips %VarName% references from a system-command
// line — a stop-the-bleed removal of the injection vector; the Suggestion
// explains proper argument passing.
func SanitizeCommandVarsPatch(block *models.Block) models.Patch {
	seen := make(map[string]bool)
	var ops []models.PatchOp
	for _, val := range block.Properties {
		for _, m := range sqlVarRef.FindAllString(val, -1) {
			if seen[m] {
				continue
			}
			seen[m] = true
			ops = append(ops, models.PatchOp{
				Kind:      "replace",
				StartLine: block.LineNumber,
				Old:       m,
				New:       "",
			})
		}
	}
	if len(ops) == 0 {
		return models.Patch{}
	}
	return models.Patch{Ops: ops}
}
