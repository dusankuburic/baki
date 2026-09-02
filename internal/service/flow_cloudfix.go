package service

import (
	"context"
	"fmt"
	"sort"

	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"
)

// ── Cloud fix alignment (R3-3) ──────────────────────────────────────────────
//
// A cloud fix patches LINE-BASED text, so the patch's line numbers must refer
// to exactly the bytes being patched. Two states satisfy that:
//
//   - STORED single-file source: Content and Source are persisted together
//     (the parse and the text come from the same upload/fix), so the parsed
//     doc's line numbers align with doc.Source.
//   - BRIDGE state (ingested padcloud flows, folder flows): no stored Source
//     exists, so canonical PAD text is DERIVED with the serializer. Canonical
//     output drops blank lines and normalizes layout — line numbers from the
//     ORIGINAL parse can be shifted, and applying them to canonical text
//     splices the WRONG LINES (the original R1-1b bridge had exactly this
//     latent bug; fixtures without blank lines masked it). The bridge
//     therefore RE-PARSES the canonical text and REGENERATES the patch
//     against that parse, relocating the target block by preorder index —
//     the serializer's round-trip gates guarantee identical tree shape.

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

// alignCloudBlock resolves the aligned patching state for blockID WITHOUT
// generating a patch: stored single-file source returns the doc itself as
// canon; bridge states (ingested, folder) canonicalize + relocate the target
// by preorder index with the shape guard. The returned context's `source`
// (single-file) or `files` (folder) is the exact text the target's line
// numbers refer to; `target` is the block to compute patches against.
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
	// Shape guard: the round-trip gates prove equivalence for the corpus, but
	// an arbitrary flow with parse errors could diverge — a RawType/Type
	// mismatch means the relocation landed on a different block, and patching
	// it would corrupt the source. Bail with an actionable error instead.
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
	patch, err := analyzer.PatchForFix(target, fixType, ruleID, variable, property)
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

// parseTextPreservingIdentity parses source and stamps the ORIGINAL doc's
// identity fields onto the result (a fresh parse mints new IDs; callers and
// storage key on the stable ones).
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

// parseFilesPreservingIdentity is parseTextPreservingIdentity for the
// multi-file (folder) shape: the parse carries per-subflow SourceFile, so
// the result remains a folder document.
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

// preorderIndexOf finds blockID's preorder position across the whole doc
// (subflows in order, blocks depth-first, END markers included — both trees
// carry them, so indices are comparable). Returns (-1, nil) when absent.
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

// applyCloudPatch applies the aligned patch and persists: folder flows patch
// the target member file and persist the reassembled folder doc; single-file
// flows patch the (stored or bridged) source directly.
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
		s.snapshotCloudFiles(doc, cctx.files, "before fix")
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
		return s.persistCloudDoc(ctx, doc, combined, "")
	}

	s.snapshotCloudSource(doc, "before fix")
	patched := analyzer.ApplyPatch(cctx.source, cctx.patch)
	updated := parseTextPreservingIdentity(patched, doc)
	if updated == nil || analyzer.CountParseErrors(updated) > analyzer.CountParseErrors(cctx.canon) {
		return nil, fmt.Errorf("patch would introduce parse errors (flow left unchanged)")
	}
	return s.persistCloudDoc(ctx, doc, updated, patched)
}

// applyFixBatchCloudFolder runs the iterative fix loop PER MEMBER FILE of a
// cloud folder flow, persisting the reassembled folder once. Per-file gate:
// a member whose fix would introduce parse errors keeps its original text
// while the rest proceed (batch semantics stay "never degrade").
func (s *FlowService) applyFixBatchCloudFolder(ctx context.Context, doc *models.FlowDocument, ruleFilter map[string]bool, limit int) (*models.FlowDocument, int, error) {
	files := parser.SerializeFiles(doc)
	if len(files) == 0 {
		return doc, 0, nil
	}
	s.snapshotCloudFiles(doc, files, "before batch fix")

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	total := 0
	skipped := 0
	fixed := copyFiles(files)
	for _, name := range names {
		src := fixed[name]
		res, err := analyzer.ApplyFixesToSourceDoc(&src, name, ruleFilter, limit, nil)
		if err != nil {
			return nil, total, err
		}
		if res.Fixed == 0 {
			continue
		}
		if res.AfterErrors > res.BeforeErrors {
			// Keep this file's original text; the rest of the batch proceeds.
			skipped += res.Fixed
			continue
		}
		fixed[name] = src
		total += res.Fixed
	}
	if total == 0 {
		return doc, 0, nil
	}
	combined := parseFilesPreservingIdentity(fixed, doc)
	if combined == nil {
		return nil, total, fmt.Errorf("re-parse fixed folder flow failed (flow left unchanged)")
	}
	updated, err := s.persistCloudDoc(ctx, doc, combined, "")
	return updated, total, err
}

func copyFiles(files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for k, v := range files {
		out[k] = v
	}
	return out
}
