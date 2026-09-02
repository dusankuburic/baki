package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"
)

// ── Block editing (R3-1b) ───────────────────────────────────────────────────
//
// RemoveBlock / DuplicateBlock are the visual-editor write operations: they
// turn a block identity into a line-range Patch (the same machinery the
// auto-fixers use) and persist through the established mutation paths —
// PatchFlow on desktop (locking, parse gate, snapshot, reload) and
// applyCloudPatch in cloud (alignment, folder member targeting, snapshot) —
// so every edit is snapshotted for undo and parse-gated against corruption.

// inlineRetryDirective reports whether a source line is an `ON ERROR REPEAT…`
// directive — the retry policy folded into the PRECEDING block's properties
// at parse time. It carries no block of its own, so a block's physical span
// must absorb it when present (delete removes it with its action; duplicate
// copies it with its action).
func inlineRetryDirective(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "ON ERROR") && strings.Contains(t, "REPEAT")
}

// spanMatchesBlock is the stale-source drift guard: the block's span was
// computed against the doc's parse, but the patch applies to bytes read from
// disk NOW. If the file changed underneath (external edit between load and
// edit), the span's start line no longer holds this block — verify the line
// still mentions the block's own action keyword before patching. Keyword
// blocks (IF/LOOP/SET/COMMENT/…) carry their keyword in RawType too, so one
// containment check covers every shape.
func spanMatchesBlock(line string, block *models.Block) bool {
	if block.RawType == "" {
		return true // nothing to verify against
	}
	return strings.Contains(line, block.RawType)
}

// blockEditPatch builds the remove/insert ops for one block against the
// given text. Span = BlockSpan + any trailing inline-retry directive lines.
// kind is "remove" or "duplicate".
func blockEditPatch(text string, block *models.Block, file, kind string) (models.Patch, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start, end := analyzer.BlockSpan(block)
	if start < 1 || end < start || end > len(lines) {
		return models.Patch{}, fmt.Errorf("block line span (%d-%d) is outside the source (%d lines) — reload the flow and retry", start, end, len(lines))
	}
	if !spanMatchesBlock(lines[start-1], block) {
		return models.Patch{}, fmt.Errorf("the flow changed on disk since it was loaded — reload the flow and retry")
	}
	// Absorb trailing inline-retry directive lines (there is at most one per
	// block in practice; the loop is cheap generality).
	for end < len(lines) && inlineRetryDirective(lines[end]) {
		end++
	}

	patch := models.Patch{File: file}
	switch kind {
	case "remove":
		patch.Ops = []models.PatchOp{{Kind: "remove", StartLine: start, EndLine: end}}
	case "duplicate":
		copied := make([]string, end-start+1)
		copy(copied, lines[start-1:end])
		patch.Ops = []models.PatchOp{{Kind: "insert", BeforeLine: end + 1, Lines: copied}}
	default:
		return models.Patch{}, fmt.Errorf("unknown block edit %q", kind)
	}
	return patch, nil
}

// RemoveBlock deletes a block (with descendants and trailing retry
// directive) from the flow source, parse-gated and snapshotted for undo.
func (s *FlowService) RemoveBlock(ctx context.Context, doc *models.FlowDocument, blockID string) (*models.FlowDocument, error) {
	return s.editBlock(ctx, doc, blockID, "remove")
}

// DuplicateBlock inserts a verbatim copy of the block (with descendants)
// directly after the original.
func (s *FlowService) DuplicateBlock(ctx context.Context, doc *models.FlowDocument, blockID string) (*models.FlowDocument, error) {
	return s.editBlock(ctx, doc, blockID, "duplicate")
}

func (s *FlowService) editBlock(ctx context.Context, doc *models.FlowDocument, blockID, kind string) (*models.FlowDocument, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	block := s.FindBlockByID(doc, blockID)
	if block == nil {
		return nil, fmt.Errorf("block %q not found", blockID)
	}

	// Desktop: the block's line numbers come from the parse of its member
	// file — read that file, build the patch, and let PatchFlow do the rest
	// (per-file lock, parse gate, snapshot, folder-aware reload).
	if doc.FilePath != "" {
		text, file, _, _, _, err := s.blockEditTargetText(doc, block, blockID)
		if err != nil {
			return nil, err
		}
		patch, err := blockEditPatch(text, block, file, kind)
		if err != nil {
			return nil, err
		}
		return s.PatchFlow(doc, patch)
	}

	// Cloud: align, patch against the CANONICAL block, persist shared path.
	cctx, target, err := s.alignCloudBlock(doc, blockID)
	if err != nil {
		return nil, err
	}
	text := cctx.source
	file := ""
	if cctx.isFolder() {
		if sf := cctx.canon.BlockSubflow[target.ID]; sf != nil && sf.SourceFile != "" {
			file = sf.SourceFile
		}
		if file == "" {
			file = "Main.txt"
		}
		t, ok := cctx.files[file]
		if !ok {
			return nil, fmt.Errorf("block targets file %q which is not part of this flow", file)
		}
		text = t
	}
	patch, err := blockEditPatch(text, target, file, kind)
	if err != nil {
		return nil, err
	}
	cctx.patch = patch
	return s.applyCloudPatch(ctx, doc, cctx)
}

// ── Bulk delete + rename (U3b power editing) ────────────────────────────────

// removeBlocksPatch builds ONE patch removing every block span (plus trailing
// retry directives) against the same text snapshot. Spans must not overlap
// (an ancestor+descendant selection refuses rather than double-deleting
// shared lines); ApplyPatch applies the removes bottom-up.
func removeBlocksPatch(text, file string, blocks []*models.Block) (models.Patch, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	type span struct{ start, end int }
	spans := make([]span, 0, len(blocks))
	for _, b := range blocks {
		start, end := analyzer.BlockSpan(b)
		if start < 1 || end < start || end > len(lines) {
			return models.Patch{}, fmt.Errorf("block line span (%d-%d) is outside the source (%d lines) — reload the flow and retry", start, end, len(lines))
		}
		if !spanMatchesBlock(lines[start-1], b) {
			return models.Patch{}, fmt.Errorf("the flow changed on disk since it was loaded — reload the flow and retry")
		}
		for end < len(lines) && inlineRetryDirective(lines[end]) {
			end++
		}
		spans = append(spans, span{start, end})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start <= spans[i-1].end {
			return models.Patch{}, fmt.Errorf("selection contains a block and its own descendant — delete the outer block instead")
		}
	}
	ops := make([]models.PatchOp, len(spans))
	for i, sp := range spans {
		ops[i] = models.PatchOp{Kind: "remove", StartLine: sp.start, EndLine: sp.end}
	}
	return models.Patch{File: file, Ops: ops}, nil
}

// memberFileFor resolves the member file a block belongs to (folder flows).
func memberFileFor(doc *models.FlowDocument, blockID string) string {
	if sf := doc.BlockSubflow[blockID]; sf != nil && sf.SourceFile != "" {
		return sf.SourceFile
	}
	return "Main.txt"
}

// RemoveBlocks deletes several blocks in ONE patch (U3b bulk delete): one
// parse gate, one snapshot, one write. All targets must live in the SAME
// member file (a cross-file batch would need one patch per file).
func (s *FlowService) RemoveBlocks(ctx context.Context, doc *models.FlowDocument, blockIDs []string) (*models.FlowDocument, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	if len(blockIDs) == 0 {
		return nil, fmt.Errorf("no blocks selected")
	}
	blocks := make([]*models.Block, 0, len(blockIDs))
	seen := map[string]bool{}
	for _, id := range blockIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		b := s.FindBlockByID(doc, id)
		if b == nil {
			return nil, fmt.Errorf("block %q not found", id)
		}
		blocks = append(blocks, b)
	}

	if doc.FilePath != "" {
		file := ""
		if info, err := os.Stat(doc.FilePath); err == nil && info.IsDir() {
			for _, b := range blocks {
				f := memberFileFor(doc, b.ID)
				if file == "" {
					file = f
				} else if file != f {
					return nil, fmt.Errorf("bulk delete targets blocks in multiple files (%q and %q) — delete them one file at a time", file, f)
				}
			}
			if file == "" {
				file = "Main.txt"
			}
			text, _, _, _, _, err := s.blockEditTargetText(doc, blocks[0], blocks[0].ID)
			if err != nil {
				return nil, err
			}
			patch, err := removeBlocksPatch(text, file, blocks)
			if err != nil {
				return nil, err
			}
			return s.PatchFlow(doc, patch)
		}
		// Single-file desktop doc.
		text, _, _, _, _, err := s.blockEditTargetText(doc, blocks[0], blocks[0].ID)
		if err != nil {
			return nil, err
		}
		patch, err := removeBlocksPatch(text, "", blocks)
		if err != nil {
			return nil, err
		}
		return s.PatchFlow(doc, patch)
	}

	// Cloud: align the FIRST block, relocate the rest by preorder index into
	// the same canon tree (the MoveBlockTo contract), then one patch.
	cctx, target, err := s.alignCloudBlock(doc, blocks[0].ID)
	if err != nil {
		return nil, err
	}
	canonBlocks := []*models.Block{target}
	for _, b := range blocks[1:] {
		idx, orig := preorderIndexOf(doc, b.ID)
		if orig == nil {
			return nil, fmt.Errorf("block %q not found", b.ID)
		}
		cb := blockAtPreorderIndex(cctx.canon, idx)
		if cb == nil || cb.RawType != orig.RawType || cb.Type != orig.Type {
			return nil, fmt.Errorf("fix unavailable: the derived source for this flow does not match its parsed structure — open the source editor and save once, then retry")
		}
		canonBlocks = append(canonBlocks, cb)
	}
	text := cctx.source
	file := ""
	if cctx.isFolder() {
		for _, b := range canonBlocks {
			f := memberFileFor(cctx.canon, b.ID)
			if file == "" {
				file = f
			} else if file != f {
				return nil, fmt.Errorf("bulk delete targets blocks in multiple files (%q and %q) — delete them one file at a time", file, f)
			}
		}
		if file == "" {
			file = "Main.txt"
		}
		t, ok := cctx.files[file]
		if !ok {
			return nil, fmt.Errorf("block targets file %q which is not part of this flow", file)
		}
		text = t
	}
	patch, err := removeBlocksPatch(text, file, canonBlocks)
	if err != nil {
		return nil, err
	}
	cctx.patch = patch
	return s.applyCloudPatch(ctx, doc, cctx)
}

// RenameBlock renames a block whose name is USER-CONTROLLED text on its
// first source line: LABEL blocks (and every same-file GOTO that targets the
// old label name) and COMMENT blocks. Action names are derived from raw type
// + properties — those refuse with a pointer to Edit properties.
// Returns the updated document and the number of GOTO references rewritten.
func (s *FlowService) RenameBlock(ctx context.Context, doc *models.FlowDocument, blockID, newName string) (*models.FlowDocument, int, error) {
	if doc == nil {
		return nil, 0, fmt.Errorf("no flow loaded")
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil, 0, fmt.Errorf("a name is required")
	}
	if strings.ContainsAny(newName, "\r\n") {
		return nil, 0, fmt.Errorf("a name cannot span multiple lines")
	}
	block := s.FindBlockByID(doc, blockID)
	if block == nil {
		return nil, 0, fmt.Errorf("block %q not found", blockID)
	}
	if block.RawType != "LABEL" && block.RawType != "COMMENT" {
		return nil, 0, fmt.Errorf("this block's name is derived from its action type and properties — use Edit properties to change what it does")
	}
	if block.RawType == "LABEL" && strings.ContainsAny(newName, "'") {
		return nil, 0, fmt.Errorf("label names cannot contain quotes")
	}
	oldName := strings.TrimSpace(block.Name)
	if oldName == "" {
		return nil, 0, fmt.Errorf("the current name could not be resolved — reload the flow and retry")
	}

	var text, file string
	var cctx *cloudFixContext
	var target *models.Block
	if doc.FilePath != "" {
		var err error
		text, file, _, target, _, err = s.blockEditTargetText(doc, block, blockID)
		if err != nil {
			return nil, 0, err
		}
	} else {
		var err error
		cctx, target, err = s.alignCloudBlock(doc, blockID)
		if err != nil {
			return nil, 0, err
		}
		text = cctx.source
		if cctx.isFolder() {
			file = memberFileFor(cctx.canon, target.ID)
			t, ok := cctx.files[file]
			if !ok {
				return nil, 0, fmt.Errorf("block targets file %q which is not part of this flow", file)
			}
			text = t
		}
	}

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start, end := analyzer.BlockSpan(target)
	if start < 1 || end > len(lines) || end < start {
		return nil, 0, fmt.Errorf("block line span is outside the source — reload the flow and retry")
	}
	if !spanMatchesBlock(lines[start-1], target) {
		return nil, 0, fmt.Errorf("the flow changed on disk since it was loaded — reload the flow and retry")
	}
	if end > start {
		return nil, 0, fmt.Errorf("this comment spans multiple lines — edit it in the source editor")
	}

	patch := models.Patch{File: file}
	gotoRefs := 0
	if target.RawType == "LABEL" {
		// Labels appear quoted in user exports ('Done') but the CANONICAL
		// serializer emits them bare — match the form the label's own line
		// uses, and accept that form on GOTO reference lines.
		oldQ, newQ := "'"+oldName+"'", "'"+newName+"'"
		useBare := !strings.Contains(lines[start-1], oldQ)
		if useBare {
			patch.Ops = append(patch.Ops, models.PatchOp{Kind: "replace", StartLine: start, Old: oldName, New: newName})
		} else {
			patch.Ops = append(patch.Ops, models.PatchOp{Kind: "replace", StartLine: start, Old: oldQ, New: newQ})
		}
		for i, ln := range lines {
			if i == start-1 {
				continue
			}
			trimmed := strings.TrimSpace(ln)
			if !strings.HasPrefix(trimmed, "GOTO ") {
				continue
			}
			if strings.Contains(ln, oldQ) {
				patch.Ops = append(patch.Ops, models.PatchOp{Kind: "replace", StartLine: i + 1, Old: oldQ, New: newQ})
				gotoRefs++
			} else if useBare && trimmed == "GOTO "+oldName {
				patch.Ops = append(patch.Ops, models.PatchOp{Kind: "replace", StartLine: i + 1, Old: oldName, New: newName})
				gotoRefs++
			}
		}
	} else {
		patch.Ops = append(patch.Ops, models.PatchOp{Kind: "replace", StartLine: start, Old: oldName, New: newName})
	}

	if cctx != nil {
		cctx.patch = patch
		updated, err := s.applyCloudPatch(ctx, doc, cctx)
		return updated, gotoRefs, err
	}
	updated, err := s.PatchFlow(doc, patch)
	return updated, gotoRefs, err
}

// blockEditTargetText resolves, for a DESKTOP doc, the CURRENT source text
// the block's line numbers refer to plus its member file (the block from the
// caller's doc — its parse read this file; "" file for single-file docs).
// Cloud callers use alignCloudBlock instead; the cctx/target/targetDoc
// returns are nil on this path (kept in the signature so property edits read
// uniformly).
func (s *FlowService) blockEditTargetText(doc *models.FlowDocument, block *models.Block, blockID string) (line, file string, cctx *cloudFixContext, target *models.Block, targetDoc *models.FlowDocument, err error) {
	targetPath := doc.FilePath
	if info, serr := os.Stat(doc.FilePath); serr == nil && info.IsDir() {
		file = "Main.txt"
		if sf := doc.BlockSubflow[blockID]; sf != nil && sf.SourceFile != "" {
			file = sf.SourceFile
		}
		targetPath = filepath.Join(doc.FilePath, file)
	}
	data, rerr := os.ReadFile(targetPath) // #nosec G304 -- derived from doc.FilePath like PatchFlow
	if rerr != nil {
		return "", "", nil, nil, nil, fmt.Errorf("read source file: %w", rerr)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n"), file, nil, block, doc, nil
}

// ── Property editing + reordering (R3-2) ─────────────────────────────────────

// isBareValue reports whether a rendered form is the unquoted one (no
// surrounding quote characters).
func isBareValue(form string) bool {
	return !strings.HasPrefix(form, "'")
}

// quotedValueForms renders v in every surface form a parsed property value
// may take in the source line, most-explicit first (PAD quotes values
// containing spaces/specials; bare is the enum/default form). Each entry is
// the FULL literal — open quotes, value, matching close quotes.
func quotedValueForms(v string) []string {
	return []string{"$'''" + v + "'''", "'''" + v + "'''", "'" + v + "'", v}
}

// locatePropertyValue finds `key: <oldvalue>` in the block's source line and
// returns the exact matched segment (key + separator + quoted old value) so a
// targeted replace can swap ONLY this property. The old value's surface form
// is tried in each quoting style — the parser unwraps any of them to the same
// parsed value. Returns ok=false when no form matches (drifted source, or the
// property is synthetic/derived) — callers refuse rather than guess.
func locatePropertyValue(line, key, oldValue string) (string, bool) {
	if key == "" || strings.ContainsAny(key, `:$`) {
		return "", false
	}
	kb := []byte(key)
	for _, old := range quotedValueForms(oldValue) {
		// Scan for the key token as a whole word (preceded by start/space,
		// followed by optional space + colon) followed by the value form.
		for i := 0; i+len(kb) <= len(line); i++ {
			if string(line[i:i+len(kb)]) != key {
				continue
			}
			if i > 0 && line[i-1] != ' ' && line[i-1] != '\t' {
				continue // mid-word hit (e.g. "Url" inside "MyUrl")
			}
			rest := line[i+len(kb):]
			j := 0
			for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
				j++
			}
			if j >= len(rest) || rest[j] != ':' {
				continue
			}
			j++
			for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
				j++
			}
			valStart := i + len(kb) + j // index in line of the value's first byte
			if !strings.HasPrefix(line[valStart:], old) {
				continue
			}
			// Bare form guard: a bare prefix could match early inside a
			// longer token (old="30" must not match "300"). The char after a
			// bare match must be a boundary: EOL or whitespace. Quoted forms
			// self-terminate at their closing quotes, so no extra check.
			segEnd := valStart + len(old)
			if isBareValue(old) && segEnd < len(line) && line[segEnd] != ' ' && line[segEnd] != '\t' {
				continue // bare match is a strict prefix of a longer token
			}
			return line[i:segEnd], true
		}
	}
	return "", false
}

// UpdateBlockProperties applies a batch of property edits to one block: each
// changed value becomes a targeted in-line replace (`Key: oldform` →
// `Key: newform`), preserving every other property's original text, order,
// and quoting on the line. Multi-line-literal blocks and `_`-prefixed
// (parser-derived) keys are refused — the source editor covers those. The
// mutation rides the established paths (parse gate, snapshot, folder/cloud
// alignment) exactly like RemoveBlock.
func (s *FlowService) UpdateBlockProperties(ctx context.Context, doc *models.FlowDocument, blockID string, changes map[string]string) (*models.FlowDocument, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	if len(changes) == 0 {
		return doc, nil
	}
	block := s.FindBlockByID(doc, blockID)
	if block == nil {
		return nil, fmt.Errorf("block %q not found", blockID)
	}
	for k := range changes {
		if strings.HasPrefix(k, "_") {
			return nil, fmt.Errorf("property %q is derived by the parser and cannot be edited here — edit the source instead", k)
		}
	}
	if block.EndLineNumber > block.LineNumber {
		return nil, fmt.Errorf("this block's value spans multiple lines — edit it in the source editor")
	}
	switch block.Type {
	case models.BlockTypeEnd, models.BlockTypeElse, models.BlockTypeCase, models.BlockTypeDefault:
		return nil, fmt.Errorf("this block type has no editable properties")
	}

	// Resolve the aligned target line (desktop: the member file; cloud:
	// stored source or the canonical bridge) exactly like editBlock.
	var text, patchFile string
	var cctx *cloudFixContext
	var target *models.Block
	if doc.FilePath != "" {
		var err error
		text, patchFile, cctx, target, _, err = s.blockEditTargetText(doc, block, blockID)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		cctx, target, err = s.alignCloudBlock(doc, blockID)
		if err != nil {
			return nil, err
		}
		if cctx.isFolder() {
			if sf := cctx.canon.BlockSubflow[target.ID]; sf != nil && sf.SourceFile != "" {
				patchFile = sf.SourceFile
			} else {
				patchFile = "Main.txt"
			}
			t, ok := cctx.files[patchFile]
			if !ok {
				return nil, fmt.Errorf("block targets file %q which is not part of this flow", patchFile)
			}
			text = t
		} else {
			text = cctx.source
		}
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if target.LineNumber < 1 || target.LineNumber > len(lines) {
		return nil, fmt.Errorf("block line is outside the source — reload the flow and retry")
	}
	fullLine := lines[target.LineNumber-1]
	if !spanMatchesBlock(fullLine, target) {
		return nil, fmt.Errorf("the flow changed on disk since it was loaded — reload the flow and retry")
	}

	patch := models.Patch{File: patchFile, Ops: []models.PatchOp{}}
	for key, newValue := range changes {
		oldSeg, ok := locatePropertyValue(fullLine, key, target.Properties[key])
		if !ok {
			return nil, fmt.Errorf("property %q could not be located on the block's source line — reload the flow and retry", key)
		}
		kb := []byte(key)
		sepEnd := strings.Index(oldSeg, ":") + 1
		for sepEnd < len(oldSeg) && (oldSeg[sepEnd] == ' ' || oldSeg[sepEnd] == '\t') {
			sepEnd++
		}
		newSeg := string(kb) + oldSeg[len(kb):sepEnd] + parser.QuoteValue(newValue)
		patch.Ops = append(patch.Ops, models.PatchOp{
			Kind: "replace", StartLine: target.LineNumber, Old: oldSeg, New: newSeg,
		})
	}
	sort.Slice(patch.Ops, func(i, j int) bool { return patch.Ops[i].Old < patch.Ops[j].Old })

	if cctx != nil {
		cctx.patch = patch
		return s.applyCloudPatch(ctx, doc, cctx)
	}
	return s.PatchFlow(doc, patch)
}

// siblingList returns the ordered non-END siblings of blockID in its parent
// scope (container children, or subflow top-level blocks), plus the target's
// index. Containers' END markers parse as children and are excluded — moving
// past an END would eject the block from its container.
func siblingList(doc *models.FlowDocument, blockID string) ([]*models.Block, int, bool) {
	// editable reports whether a parsed child is a movable sibling (structural
	// markers are not).
	editable := func(b *models.Block) bool {
		switch b.Type {
		case models.BlockTypeEnd, models.BlockTypeElse, models.BlockTypeCase, models.BlockTypeDefault:
			return false
		}
		return true
	}

	// findScope walks depth-first; when a scope contains the target it
	// returns that scope's editable siblings in order.
	var findScope func(blocks []models.Block) ([]*models.Block, bool)
	findScope = func(blocks []models.Block) ([]*models.Block, bool) {
		var out []*models.Block
		contains := false
		for i := range blocks {
			b := &blocks[i]
			if editable(b) {
				out = append(out, b)
			}
			if b.ID == blockID {
				contains = true
			}
		}
		if contains {
			return out, true
		}
		for i := range blocks {
			if kids, ok := findScope(blocks[i].Children); ok {
				return kids, true
			}
		}
		return nil, false
	}

	for i := range doc.Subflows {
		if sibs, ok := findScope(doc.Subflows[i].Blocks); ok {
			for idx, b := range sibs {
				if b.ID == blockID {
					return sibs, idx, true
				}
			}
		}
	}
	return nil, -1, false
}

// MoveBlock reorders a block among its siblings ("up" = before the previous
// sibling, "down" = after the next). The move is a remove + verbatim insert
// patch pair — ApplyPatch applies removes first, so the insert's BeforeLine
// is computed in POST-REMOVAL coordinates. Parse-gated and snapshotted like
// every block edit; containers carry their whole span (children + END).
func (s *FlowService) MoveBlock(ctx context.Context, doc *models.FlowDocument, blockID, direction string) (*models.FlowDocument, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	if direction != "up" && direction != "down" {
		return nil, fmt.Errorf("direction must be \"up\" or \"down\"")
	}
	block := s.FindBlockByID(doc, blockID)
	if block == nil {
		return nil, fmt.Errorf("block %q not found", blockID)
	}

	// Alignment: desktop reads the member file; cloud bridges canonically.
	// Sibling enumeration happens on the SAME doc the line numbers refer to
	// (desktop: the caller's doc — its parse read this file; cloud bridge:
	// the canon re-parse).
	var moveDoc *models.FlowDocument
	var patchFile string
	var cctx *cloudFixContext
	if doc.FilePath == "" {
		var target *models.Block
		var err error
		cctx, target, err = s.alignCloudBlock(doc, blockID)
		if err != nil {
			return nil, err
		}
		moveDoc = cctx.canon
		if cctx.isFolder() {
			if sf := cctx.canon.BlockSubflow[target.ID]; sf != nil && sf.SourceFile != "" {
				patchFile = sf.SourceFile
			} else {
				patchFile = "Main.txt"
			}
		}
		block = target
	} else {
		moveDoc = doc
		if info, err := os.Stat(doc.FilePath); err == nil && info.IsDir() {
			if sf := doc.BlockSubflow[blockID]; sf != nil && sf.SourceFile != "" {
				patchFile = sf.SourceFile
			} else {
				patchFile = "Main.txt"
			}
		}
	}

	siblings, index, ok := siblingList(moveDoc, block.ID)
	if !ok {
		return nil, fmt.Errorf("block not found in the flow tree")
	}

	// Resolve the source text the spans refer to.
	text := ""
	if cctx != nil {
		if cctx.isFolder() {
			t, exists := cctx.files[patchFile]
			if !exists {
				return nil, fmt.Errorf("block targets file %q which is not part of this flow", patchFile)
			}
			text = t
		} else {
			text = cctx.source
		}
	} else {
		targetPath := doc.FilePath
		if patchFile != "" {
			targetPath = filepath.Join(doc.FilePath, patchFile)
		}
		data, err := os.ReadFile(targetPath) // #nosec G304 -- derived from doc.FilePath like PatchFlow
		if err != nil {
			return nil, fmt.Errorf("read source file: %w", err)
		}
		text = string(data)
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	spanOf := func(b *models.Block) (int, int) {
		start, end := analyzer.BlockSpan(b)
		for end < len(lines) && inlineRetryDirective(lines[end]) {
			end++
		}
		return start, end
	}

	bs, be := spanOf(block)
	if bs < 1 || be < bs || be > len(lines) {
		return nil, fmt.Errorf("block line span (%d-%d) is outside the source — reload the flow and retry", bs, be)
	}
	if !spanMatchesBlock(lines[bs-1], block) {
		return nil, fmt.Errorf("the flow changed on disk since it was loaded — reload the flow and retry")
	}

	removed := be - bs + 1
	var insertBefore int
	switch direction {
	case "up":
		if index == 0 {
			return nil, fmt.Errorf("the block is already first in its scope")
		}
		prev := siblings[index-1]
		ps, _ := spanOf(prev)
		if !spanMatchesBlock(lines[ps-1], prev) {
			return nil, fmt.Errorf("the flow changed on disk since it was loaded — reload the flow and retry")
		}
		// prev is entirely above the moved block: its lines are unshifted by
		// the removal, so inserting before ps lands above it.
		insertBefore = ps
	case "down":
		if index >= len(siblings)-1 {
			return nil, fmt.Errorf("the block is already last in its scope")
		}
		next := siblings[index+1]
		ns, ne := spanOf(next)
		if !spanMatchesBlock(lines[ns-1], next) {
			return nil, fmt.Errorf("the flow changed on disk since it was loaded — reload the flow and retry")
		}
		// next lies entirely below the moved block: shift its end up by the
		// removed line count, then insert after it (BeforeLine = end + 1).
		insertBefore = ne - removed + 1
	}

	copied := make([]string, removed)
	copy(copied, lines[bs-1:be])
	patch := models.Patch{
		File: patchFile,
		Ops: []models.PatchOp{
			{Kind: "remove", StartLine: bs, EndLine: be},
			{Kind: "insert", BeforeLine: insertBefore, Lines: copied},
		},
	}

	if cctx != nil {
		cctx.patch = patch
		return s.applyCloudPatch(ctx, doc, cctx)
	}
	return s.PatchFlow(doc, patch)
}

// MoveBlockTo reorders a block relative to a REFERENCE sibling ("before" or
// "after" it) — the atomic primitive a drag-and-drop maps to (up/down can't
// express a multi-position move). Re-parenting is out of scope by design:
// the reference must be a SIBLING of the moved block (same scope), else the
// edit is refused — moving into/out of a container changes tree semantics
// (indentation, variable scope) and is the L-sized freeform-reorder story.
func (s *FlowService) MoveBlockTo(ctx context.Context, doc *models.FlowDocument, blockID, refBlockID, position string) (*models.FlowDocument, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	if position != "before" && position != "after" {
		return nil, fmt.Errorf("position must be \"before\" or \"after\"")
	}
	if blockID == refBlockID {
		return nil, fmt.Errorf("a block cannot be moved relative to itself")
	}
	block := s.FindBlockByID(doc, blockID)
	if block == nil {
		return nil, fmt.Errorf("block %q not found", blockID)
	}

	// Alignment mirrors MoveBlock: desktop patches the caller's doc against
	// the member file; cloud bridges to the canonical re-parse.
	var moveDoc *models.FlowDocument
	var patchFile string
	var cctx *cloudFixContext
	if doc.FilePath == "" {
		var target *models.Block
		var err error
		cctx, target, err = s.alignCloudBlock(doc, blockID)
		if err != nil {
			return nil, err
		}
		moveDoc = cctx.canon
		if cctx.isFolder() {
			if sf := cctx.canon.BlockSubflow[target.ID]; sf != nil && sf.SourceFile != "" {
				patchFile = sf.SourceFile
			} else {
				patchFile = "Main.txt"
			}
		}
		block = target
		// Bridge re-parses mint fresh block IDs: relocate the REFERENCE block
		// from the caller's doc into the canon tree by preorder index (the
		// same relocation+shape-guard contract as the moved block).
		idx, orig := preorderIndexOf(doc, refBlockID)
		if orig == nil {
			return nil, fmt.Errorf("reference block %q not found", refBlockID)
		}
		ref := blockAtPreorderIndex(cctx.canon, idx)
		if ref == nil || ref.RawType != orig.RawType || ref.Type != orig.Type {
			return nil, fmt.Errorf("fix unavailable: the derived source for this flow does not match its parsed structure — open the source editor and save once, then retry")
		}
		refBlockID = ref.ID
	} else {
		moveDoc = doc
		if info, err := os.Stat(doc.FilePath); err == nil && info.IsDir() {
			if sf := doc.BlockSubflow[blockID]; sf != nil && sf.SourceFile != "" {
				patchFile = sf.SourceFile
			} else {
				patchFile = "Main.txt"
			}
		}
	}

	// Sibling constraint: both blocks must live in the SAME scope.
	siblings, index, ok := siblingList(moveDoc, block.ID)
	if !ok {
		return nil, fmt.Errorf("block not found in the flow tree")
	}
	refIndex := -1
	for i, b := range siblings {
		if b.ID == refBlockID {
			refIndex = i
			break
		}
	}
	var ref *models.Block
	if refIndex >= 0 {
		ref = siblings[refIndex]
		// No-op positions refuse (before-the-next == after-self is a no-op).
		if position == "before" && refIndex == index+1 {
			return nil, fmt.Errorf("the block is already there")
		}
		if position == "after" && refIndex == index-1 {
			return nil, fmt.Errorf("the block is already there")
		}
	} else {
		// Cross-scope re-parent: the reference lives in another container.
		// The same remove+insert composes the move; the copied lines are
		// re-indented to the reference's depth (below). Structural markers
		// stay invalid anchors — dropping next to one is ambiguous.
		ref = s.FindBlockByID(moveDoc, refBlockID)
		if ref == nil {
			return nil, fmt.Errorf("reference block %q not found", refBlockID)
		}
		switch ref.Type {
		case models.BlockTypeEnd, models.BlockTypeElse, models.BlockTypeCase, models.BlockTypeDefault:
			return nil, fmt.Errorf("blocks cannot be moved relative to a structural marker — pick a real block as the drop target")
		}
	}

	// Resolve the source text (member file on desktop, canon on cloud).
	var text string
	if cctx != nil {
		if cctx.isFolder() {
			t, exists := cctx.files[patchFile]
			if !exists {
				return nil, fmt.Errorf("block targets file %q which is not part of this flow", patchFile)
			}
			text = t
		} else {
			text = cctx.source
		}
	} else {
		targetPath := doc.FilePath
		if patchFile != "" {
			targetPath = filepath.Join(doc.FilePath, patchFile)
		}
		data, err := os.ReadFile(targetPath) // #nosec G304 -- derived from doc.FilePath like PatchFlow
		if err != nil {
			return nil, fmt.Errorf("read source file: %w", err)
		}
		text = string(data)
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	spanOf := func(b *models.Block) (int, int, error) {
		start, end := analyzer.BlockSpan(b)
		for end < len(lines) && inlineRetryDirective(lines[end]) {
			end++
		}
		if start < 1 || end < start || end > len(lines) {
			return 0, 0, fmt.Errorf("block line span (%d-%d) is outside the source — reload the flow and retry", start, end)
		}
		if !spanMatchesBlock(lines[start-1], b) {
			return 0, 0, fmt.Errorf("the flow changed on disk since it was loaded — reload the flow and retry")
		}
		return start, end, nil
	}

	bs, be, err := spanOf(block)
	if err != nil {
		return nil, err
	}
	rs, re, err := spanOf(ref)
	if err != nil {
		return nil, err
	}

	removed := be - bs + 1
	var insertBefore int
	switch {
	case re < bs: // ref entirely above: unshifted by the removal
		if position == "before" {
			insertBefore = rs
		} else {
			insertBefore = re + 1
		}
	case rs > be: // ref entirely below: shift up by the removed count
		if position == "before" {
			insertBefore = rs - removed
		} else {
			insertBefore = re - removed + 1
		}
	default:
		// A reference whose whole span lies INSIDE the moved span is the
		// re-parent-into-own-subtree case (only possible cross-scope).
		if rs >= bs && re <= be {
			return nil, fmt.Errorf("a block cannot be moved into itself or its own descendants")
		}
		return nil, fmt.Errorf("block spans overlap — the flow is in an unexpected state; reload and retry")
	}

	copied := make([]string, removed)
	copy(copied, lines[bs-1:be])
	// Cross-scope: shift the copied subtree's indentation to the reference's
	// depth so the re-parse nests it under the reference's parent. Interior
	// blank lines stay blank; multi-line literals shift with their block (the
	// same trade-off the wrap fixer's IndentDelta makes).
	if refIndex < 0 {
		copied = reindentLines(copied, ref.Indent-block.Indent)
	}
	patch := models.Patch{
		File: patchFile,
		Ops: []models.PatchOp{
			{Kind: "remove", StartLine: bs, EndLine: be},
			{Kind: "insert", BeforeLine: insertBefore, Lines: copied},
		},
	}

	if cctx != nil {
		cctx.patch = patch
		return s.applyCloudPatch(ctx, doc, cctx)
	}
	return s.PatchFlow(doc, patch)
}

// reindentLines shifts each line's leading whitespace by delta COLUMNS
// (Block.Indent units: space=1, tab=4 — so one nesting level is 4). Blank
// lines stay blank; an outdent never eats past the first non-whitespace byte.
func reindentLines(lines []string, delta int) []string {
	if delta == 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		switch {
		case ln == "":
			out[i] = ln
		case delta > 0:
			out[i] = strings.Repeat(" ", delta) + ln
		default:
			out[i] = outdentColumns(ln, -delta)
		}
	}
	return out
}

// outdentColumns strips up to cols leading whitespace columns from ln. A tab
// that straddles the target column is dropped and its surviving columns
// re-emitted as spaces, so the text after it keeps its physical position.
func outdentColumns(ln string, cols int) string {
	i, seen := 0, 0
	for i < len(ln) && seen < cols {
		switch ln[i] {
		case ' ':
			seen++
			i++
		case '\t':
			if seen+4 <= cols {
				seen += 4
				i++
				continue
			}
			return strings.Repeat(" ", seen+4-cols) + ln[i+1:]
		default:
			return ln[i:]
		}
	}
	return ln[i:]
}
