package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"
)

// Cloud fix alignment (R3-3): a cloud fix patches LINE-BASED text, so the patch's
// line numbers must refer to exactly the bytes being patched. Stored single-file
// source is aligned by construction (Content and Source persist together); bridge
// states (bulk-imported, folder) derive canonical text with the serializer — whose
// output drops blank lines and normalizes layout, so original-parse line numbers
// would splice the WRONG LINES (the R1-1b bridge's latent bug, masked by fixtures
// without blank lines) — and therefore re-parse the canonical text and regenerate
// the patch against that parse, relocating the target by preorder index (the
// serializer's round-trip gates guarantee identical tree shape).

// cloudFixContext is the aligned patching state for one cloud fix.
type cloudFixContext struct {
	// files is the per-file canonical source for FOLDER flows (nil for
	// single-file). Keyed by filename; patch.File selects the target.
	files map[string]string
	// source is the single-file text the patch's lines refer to ("" for folders).
	source string
	// canon is the parse of EXACTLY the above bytes — the patch is generated
	// from this tree, not from the caller's doc.
	canon *models.FlowDocument
	// patch is the (regenerated) patch, aligned with canon.
	patch models.Patch
}

func (c *cloudFixContext) isFolder() bool { return c.files != nil }

// alignCloudBlock resolves the aligned patching state for blockID WITHOUT generating
// a patch: stored single-file source returns the doc itself as canon; bridge states
// (ingested, folder) canonicalize + relocate the target by preorder index with the
// shape guard. The returned `source` (single-file) or `files` (folder) is the exact
// text the target's line numbers refer to; `target` is the block to patch against.
func (s *FlowService) alignCloudBlock(doc *models.FlowDocument, blockID string) (*cloudFixContext, *models.Block, error) {
	if doc == nil {
		return nil, nil, fmt.Errorf("no flow loaded")
	}

	// Stored single-file source: already aligned (see above).
	if !doc.IsFolder && doc.Source != "" {
		blk := s.FindBlockByID(doc, blockID)
		if blk == nil {
			return nil, nil, fmt.Errorf("block %q not found", blockID)
		}
		return &cloudFixContext{source: doc.Source, canon: doc}, blk, nil
	}

	// Bridge: canonicalize, re-parse, relocate.
	var files map[string]string
	var combined string
	var canon *models.FlowDocument
	if doc.IsFolder {
		files = parser.SerializeFiles(doc)
		canon = parseFilesPreservingIdentity(files, doc)
	} else {
		combined = parser.SerializeDocument(doc)
		canon = parseTextPreservingIdentity(combined, doc)
	}
	if canon == nil {
		return nil, nil, fmt.Errorf("could not derive source for this flow")
	}

	idx, orig := preorderIndexOf(doc, blockID)
	if orig == nil {
		return nil, nil, fmt.Errorf("block %q not found", blockID)
	}
	target := blockAtPreorderIndex(canon, idx)
	// Shape guard: an arbitrary flow with parse errors could diverge from what the
	// round-trip gates prove for the corpus — a RawType/Type mismatch means the
	// relocation landed on a different block, and patching it would corrupt the
	// source. Bail with an actionable error instead.
	if target == nil || target.RawType != orig.RawType || target.Type != orig.Type {
		return nil, nil, fmt.Errorf("fix unavailable: the derived source for this flow does not match its parsed structure — open the source editor and save once, then retry")
	}
	return &cloudFixContext{files: files, source: combined, canon: canon}, target, nil
}

// cloudFixContextFor builds the aligned context (with patch) for a fix.
func (s *FlowService) cloudFixContextFor(doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (*cloudFixContext, error) {
	cctx, target, err := s.alignCloudBlock(doc, blockID)
	if err != nil {
		return nil, err
	}
	patch, err := patchForFix(target, fixType, ruleID, variable, property)
	if err != nil {
		return nil, err
	}
	if cctx.isFolder() {
		if sf := cctx.canon.BlockSubflow[target.ID]; sf != nil && sf.SourceFile != "" {
			patch.File = sf.SourceFile
		}
	}
	cctx.patch = patch
	return cctx, nil
}

// parseTextPreservingIdentity parses source and stamps the ORIGINAL doc's identity
// fields onto the result (a fresh parse mints new IDs; callers and storage key on
// the stable ones).
func parseTextPreservingIdentity(source string, doc *models.FlowDocument) *models.FlowDocument {
	parsed, err := parser.ParseText(source, doc.Name, int64(len(source)))
	if err != nil {
		return nil
	}
	parsed.ID = doc.ID
	parsed.Name = doc.Name
	parsed.OwnerID = doc.OwnerID
	parsed.OrganizationID = doc.OrganizationID
	parsed.FilePath = doc.FilePath
	parsed.RebuildIndexes()
	return parsed
}

// parseFilesPreservingIdentity is parseTextPreservingIdentity for the multi-file
// (folder) shape: the parse carries per-subflow SourceFile, so the result stays a
// folder document.
func parseFilesPreservingIdentity(files map[string]string, doc *models.FlowDocument) *models.FlowDocument {
	parsed, err := parser.ParseFiles(files, doc.Name)
	if err != nil {
		return nil
	}
	parsed.ID = doc.ID
	parsed.Name = doc.Name
	parsed.OwnerID = doc.OwnerID
	parsed.OrganizationID = doc.OrganizationID
	parsed.FilePath = doc.FilePath
	parsed.IsFolder = true
	parsed.RebuildIndexes()
	return parsed
}

// preorderIndexOf finds blockID's preorder position across the whole doc (subflows
// in order, blocks depth-first, END markers included — both trees carry them, so
// indices are comparable); (-1, nil) when absent.
func preorderIndexOf(doc *models.FlowDocument, blockID string) (int, *models.Block) {
	idx := 0
	var found *models.Block
	var walk func(blocks []models.Block) bool
	walk = func(blocks []models.Block) bool {
		for i := range blocks {
			if blocks[i].ID == blockID {
				found = &blocks[i]
				return true
			}
			idx++
			if walk(blocks[i].Children) {
				return true
			}
		}
		return false
	}
	for i := range doc.Subflows {
		if walk(doc.Subflows[i].Blocks) {
			break
		}
	}
	if found == nil {
		return -1, nil
	}
	return idx, found
}

// blockAtPreorderIndex returns the block at the given preorder position, or
// nil when the index runs past the tree.
func blockAtPreorderIndex(doc *models.FlowDocument, want int) *models.Block {
	idx := 0
	var hit *models.Block
	var walk func(blocks []models.Block) bool
	walk = func(blocks []models.Block) bool {
		for i := range blocks {
			if idx == want {
				hit = &blocks[i]
				return true
			}
			idx++
			if walk(blocks[i].Children) {
				return true
			}
		}
		return false
	}
	for i := range doc.Subflows {
		if walk(doc.Subflows[i].Blocks) {
			break
		}
	}
	return hit
}

// applyCloudPatch applies the aligned patch and persists: folder flows patch the
// target member file and persist the reassembled folder doc; single-file flows patch
// the (stored or bridged) source directly.
func (s *FlowService) applyCloudPatch(ctx context.Context, doc *models.FlowDocument, cctx *cloudFixContext) (*models.FlowDocument, error) {
	if cctx.isFolder() {
		fname := cctx.patch.File
		if fname == "" {
			fname = "Main.txt"
		}
		text, ok := cctx.files[fname]
		if !ok {
			return nil, fmt.Errorf("patch targets file %q which is not part of this flow", fname)
		}
		patched := analyzer.ApplyPatch(text, cctx.patch)
		beforeDoc, berr := parser.ParseText(text, fname, int64(len(text)))
		afterDoc, aerr := parser.ParseText(patched, fname, int64(len(patched)))
		if aerr != nil || berr != nil || analyzer.CountParseErrors(afterDoc) > analyzer.CountParseErrors(beforeDoc) {
			return nil, fmt.Errorf("patch would introduce parse errors (flow left unchanged)")
		}
		files := copyFiles(cctx.files)
		files[fname] = patched
		combined := parseFilesPreservingIdentity(files, doc)
		if combined == nil {
			return nil, fmt.Errorf("re-parse patched folder flow failed (flow left unchanged)")
		}
		// cctx.files is the untouched pre-fix text (the patch went into a copy); the
		// undo entry is captured past the gates and recorded only on persist success
		// (a rejected fix used to cost an undo slot and a full folder copy).
		snap := s.snapshotCloudFiles(doc, cctx.files, "before fix")
		updated, err := s.persistCloudDoc(ctx, doc, combined, "")
		if err != nil {
			return nil, err
		}
		s.pushSnapshot(doc, snap)
		return updated, nil
	}

	patched := analyzer.ApplyPatch(cctx.source, cctx.patch)
	updated := parseTextPreservingIdentity(patched, doc)
	if updated == nil || analyzer.CountParseErrors(updated) > analyzer.CountParseErrors(cctx.canon) {
		return nil, fmt.Errorf("patch would introduce parse errors (flow left unchanged)")
	}
	// Same as the folder branch: capture past the gate, record on success.
	// cctx.source is exactly the derivation the patch ran against (stored source,
	// or the canonical serialization built for an ingested flow).
	snap := s.snapshotCloudSourceText(doc, cctx.source, "before fix")
	persisted, err := s.persistCloudDoc(ctx, doc, updated, patched)
	if err != nil {
		return nil, err
	}
	s.pushSnapshot(doc, snap)
	return persisted, nil
}

// applyFixBatchCloudFolder runs the iterative fix loop PER MEMBER FILE of a cloud
// folder flow, persisting the reassembled folder once. Per-file gate: a member whose
// fix would introduce parse errors keeps its original text while the rest proceed
// ("never degrade"). A deadline mid-batch keeps the members fixed so far, persists
// them under a detached context and reports Partial — the desktop folder path's
// contract (applyFixBatchDesktopFolder).
func (s *FlowService) applyFixBatchCloudFolder(ctx context.Context, doc *models.FlowDocument, ruleFilter map[string]bool, limit int) (FixBatchResult, error) {
	files := parser.SerializeFiles(doc)
	if len(files) == 0 {
		return FixBatchResult{Doc: doc}, nil
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	total := 0
	skipped := 0
	fixed := copyFiles(files)
	// Resolved once for the whole folder: every member file belongs to the same
	// flow, so they share one org profile.
	opts := s.fixOptions(ctx, doc, ruleFilter, limit)
	var cutShort error // ctx's error once it ended the batch early
	for _, name := range names {
		src := fixed[name]
		res, err := analyzer.ApplyFixesToSourceDocCtx(ctx, &src, name, opts)
		if err != nil {
			if !res.Partial {
				// This member got no fix: ctx ran out before its first one, or it
				// failed outright. Earlier members' fixes survive a deadline; a
				// real failure still fails the batch.
				if cerr := ctx.Err(); cerr == nil || !errors.Is(err, cerr) || total == 0 {
					return FixBatchResult{Applied: total}, err
				}
				cutShort = err
				break
			}
			cutShort = err // this member's fixes are usable; stop after it
		}
		if res.Fixed > 0 {
			if res.AfterErrors > res.BeforeErrors {
				// Keep this file's original text; the rest of the batch proceeds.
				skipped += res.Fixed
			} else {
				fixed[name] = src
				total += res.Fixed
			}
		}
		if cutShort != nil {
			break
		}
	}
	if total == 0 {
		if cutShort != nil {
			return FixBatchResult{}, cutShort
		}
		return FixBatchResult{Doc: doc}, nil
	}
	combined := parseFilesPreservingIdentity(fixed, doc)
	if combined == nil {
		return FixBatchResult{Applied: total}, fmt.Errorf("re-parse fixed folder flow failed (flow left unchanged)")
	}
	// A batch cut short by the request deadline persists what it applied under a
	// detached, bounded context: the request context is already done, and a save
	// under it would fail (and discard the work).
	persistCtx := ctx
	if cutShort != nil {
		var cancel context.CancelFunc
		persistCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), partialPersistTimeout)
		defer cancel()
	}
	// files is the untouched pre-batch text (the loop fixed a copy). The snapshot
	// used to be taken before the loop, so a no-op batch, failed re-parse or
	// rejected save still consumed an undo slot; it is now captured here and
	// recorded only once the persist succeeded.
	snap := s.snapshotCloudFiles(doc, files, "before batch fix")
	updated, err := s.persistCloudDoc(persistCtx, doc, combined, "")
	if err != nil {
		return FixBatchResult{Applied: total}, err
	}
	s.pushSnapshot(doc, snap)
	return FixBatchResult{Doc: updated, Applied: total, Partial: cutShort != nil, Reason: cutShort}, nil
}

func copyFiles(files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for k, v := range files {
		out[k] = v
	}
	return out
}
