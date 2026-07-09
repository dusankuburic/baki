package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-core/logger"
	"pad-core/models"
	"pad-core/parser"
	"pad-core/search"
)

// FlowService owns document state, search index, and all file-related operations.
type FlowService struct {
	notifier    Notifier
	settings    SettingsProvider
	docProvider DocumentProvider
	storage     storageif.StorageBackend
	authz       *AuthzService
	astCache    cache.Cache

	// idxCache memoises built search indexes by flow ID. Building an index walks
	// and tokenises every block, so rebuilding it on every search-as-you-type
	// keystroke is wasteful; a flow's content is immutable for a given ID
	// (a new upload/parse yields a new ID), and in-place cloud updates call
	// InvalidateSearchIndex. It is a bounded LRU so a long-lived process opening
	// many distinct flows can't grow it without limit.
	idxCache cache.Cache

	// invalidateCbs holds flow-invalidation callbacks registered by other
	// services (e.g. ChatService's scrubbed-context cache) that must be dropped
	// when a flow changes in place. Registering here avoids a direct dependency
	// from LibraryService → ChatService (which would cycle, since ChatService
	// already depends on FlowService).
	invalidateMu  sync.Mutex
	invalidateCbs []func(flowID string)
}

// maxSearchIndexCache bounds the number of cached search indexes (one per
// distinct flow). Each index is roughly proportional to block count, so this
// caps worst-case memory regardless of how many flows are opened over uptime.
const maxSearchIndexCache = 64

func NewFlowService(notifier Notifier, settings SettingsProvider, docProvider DocumentProvider, storage storageif.StorageBackend, authz *AuthzService, astCache cache.Cache) *FlowService {
	idxCache, _ := cache.NewLRUCache(maxSearchIndexCache) // size > 0 ⇒ error impossible
	return &FlowService{
		notifier:    notifier,
		settings:    settings,
		docProvider: docProvider,
		storage:     storage,
		authz:       authz,
		astCache:    astCache,
		idxCache:    idxCache,
	}
}

// GetAuthorized loads a flow and verifies the user has at least minPerm access.
// minPerm is "viewer", "editor", or "admin". All policy lives in AuthzService.
func (s *FlowService) GetAuthorized(ctx context.Context, flowID, userID, minPerm string) (*models.FlowDocument, error) {
	if s.storage == nil { // Local mode
		return s.docProvider.ResolveDoc(ctx, flowID)
	}

	doc, err := s.docProvider.ResolveDoc(ctx, flowID)
	if err != nil {
		return nil, err
	}

	if err := s.authz.CheckFlowAccess(ctx, flowID, doc.OwnerID, doc.OrganizationID, userID, minPerm); err != nil {
		return nil, err
	}
	return doc, nil
}

func (s *FlowService) DocProvider() DocumentProvider {
	return s.docProvider
}

func (s *FlowService) InvalidateSearchIndex(flowID string) {
	s.idxCache.Delete(context.Background(), flowID)
	// Fan out to any registered derived caches (e.g. ChatService's scrubbed
	// context) so an in-place flow edit invalidates them too. Snapshot under
	// the lock so a callback may register another callback without deadlock.
	s.invalidateMu.Lock()
	cbs := append([]func(string){}, s.invalidateCbs...)
	s.invalidateMu.Unlock()
	for _, cb := range cbs {
		cb(flowID)
	}
}

// OnInvalidateFlow registers a callback invoked from InvalidateSearchIndex
// whenever a flow's derived caches must be dropped. See FlowService doc comment.
func (s *FlowService) OnInvalidateFlow(cb func(flowID string)) {
	s.invalidateMu.Lock()
	defer s.invalidateMu.Unlock()
	s.invalidateCbs = append(s.invalidateCbs, cb)
}

func (s *FlowService) FindBlockByID(doc *models.FlowDocument, blockID string) *models.Block {
	if doc == nil || blockID == "" {
		return nil
	}
	if b, ok := doc.BlocksByID[blockID]; ok {
		return b
	}
	return nil
}

func (s *FlowService) FindSubflowForBlock(doc *models.FlowDocument, blockID string) *models.Subflow {
	if doc == nil || blockID == "" {
		return nil
	}
	if sf, ok := doc.BlockSubflow[blockID]; ok {
		return sf
	}
	return nil
}

func (s *FlowService) LoadFlowFromPath(path string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFromPath", &err)
	if err := validateUserPath(path); err != nil {
		return nil, err
	}
	doc, err = s.loadAndParse(path)
	if err == nil {
		s.docProvider.SetCurrentDoc(doc)
	}
	return doc, err
}

func (s *FlowService) LoadFlowFolder(folderPath string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFolder", &err)

	if err := validateUserPath(folderPath); err != nil {
		return nil, err
	}
	if s.settings == nil {
		return nil, fmt.Errorf("application not fully initialized")
	}
	maxSizeMB := s.settings.Get().Parser.MaxFileSizeMB
	maxSize := int64(maxSizeMB) * 1024 * 1024
	entries, readErr := os.ReadDir(folderPath)
	if readErr != nil {
		return nil, fmt.Errorf("read folder: %w", readErr)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".txt") {
			continue
		}
		info, statErr := e.Info()
		if statErr == nil && maxSize > 0 && info.Size() > maxSize {
			return nil, fmt.Errorf("file %s too large (%d MB). Max is %d MB", e.Name(), info.Size()/(1024*1024), maxSizeMB)
		}
	}

	doc, err = parser.ParseFolder(folderPath)
	if err != nil {
		return nil, err
	}

	if s.settings != nil {
		totalSize := doc.Metadata.FileSize
		_ = s.settings.AddRecentFile(folderPath, totalSize)
	}

	s.docProvider.SetCurrentDoc(doc)
	return doc, nil
}

// LoadAllFromFolder parses every .txt flow export in folderPath. Files that
// fail to read or parse are skipped but reported in loadErrors (filename →
// reason) so batch analysis can show them instead of silently dropping them.
func (s *FlowService) LoadAllFromFolder(ctx context.Context, folderPath string) ([]*models.FlowDocument, map[string]string, error) {
	if err := validateUserPath(folderPath); err != nil {
		return nil, nil, err
	}

	entries, err := os.ReadDir(folderPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read folder: %w", err)
	}

	var docs []*models.FlowDocument
	loadErrors := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if !strings.HasSuffix(name, ".txt") {
			continue
		}

		filePath := filepath.Join(folderPath, e.Name())
		data, err := os.ReadFile(filePath) // #nosec G304 -- reading flow files from the folder the user opened
		if err != nil {
			loadErrors[e.Name()] = "read failed: " + err.Error()
			continue
		}

		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		doc, err := parser.ParseText(string(data), e.Name(), size)
		if err != nil {
			loadErrors[e.Name()] = "parse failed: " + err.Error()
			continue
		}
		// Give the doc its on-disk identity: session analytics key on FilePath
		// (analyzer.StableFlowID), so batch-analyzing the same file twice — or
		// batching a file already analyzed via a single load — updates one entry
		// instead of double-counting under two fresh UUIDs.
		doc.FilePath = filePath

		// The parser wraps loose content in an implicit subflow, so emptiness
		// shows up as zero blocks rather than zero subflows.
		if len(doc.Subflows) == 0 || doc.Metadata.BlockCount == 0 {
			loadErrors[e.Name()] = "no flow content found (not a PAD flow export?)"
			continue
		}
		docs = append(docs, doc)
	}

	if len(docs) == 0 && len(loadErrors) == 0 {
		return nil, nil, fmt.Errorf("no flow files found in folder")
	}
	return docs, loadErrors, nil
}

func (s *FlowService) LoadFlowFiles(ctx context.Context, files map[string]string, rootName string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFiles", &err)

	// Generate a combined hash of all files to use as a cache key
	h := sha256.New()
	// Sort keys for a deterministic hash — Go's map iteration order is random,
	// so without this the same upload hashes differently and never cache-hits.
	fileNames := make([]string, 0, len(files))
	for k := range files {
		fileNames = append(fileNames, k)
	}
	sort.Strings(fileNames)
	for _, name := range fileNames {
		h.Write([]byte(name))
		h.Write([]byte(files[name]))
	}
	key := "ast-files:" + hex.EncodeToString(h.Sum(nil))

	if s.astCache != nil {
		if cached, ok := s.astCache.Get(ctx, key); ok {
			if doc, ok := cached.(*models.FlowDocument); ok {
				s.docProvider.SetCurrentDoc(doc)
				// NOTE: Emit (broadcast) is safe here — LoadFlowFiles is a desktop-only
				// code path that reads from the local filesystem. Cloud mode loads flows
				// from Postgres via separate handlers. If this ever becomes reachable from
				// a JWT/cloud handler, switch to EmitTo with the user's ID to prevent
				// cross-tenant data leaks.
				s.notifier.Emit("flow:loaded", doc)
				logger.Info("flow files loaded from cache", "root", rootName)
				return doc, nil
			}
		}
	}

	doc, err = parser.ParseFiles(files, rootName)
	if err != nil {
		return nil, err
	}

	if s.astCache != nil {
		s.astCache.Set(ctx, key, doc, 24*time.Hour)
	}

	s.docProvider.SetCurrentDoc(doc)
	s.notifier.Emit("flow:loaded", doc)
	return doc, nil
}

func (s *FlowService) RecentFiles() (files []models.RecentFile, err error) {
	defer logger.Guard("App.RecentFiles", &err)
	if s.settings == nil {
		return nil, nil
	}
	st := s.settings.Get()
	return st.RecentFiles, nil
}

func (s *FlowService) RemoveRecentFile(path string) (err error) {
	defer logger.Guard("App.RemoveRecentFile", &err)
	return s.settings.RemoveRecentFile(path)
}

func (s *FlowService) ClearRecentFiles() (err error) {
	defer logger.Guard("App.ClearRecentFiles", &err)
	return s.settings.ClearRecentFiles()
}

func (s *FlowService) RevealInFileManager(path string) (err error) {
	defer logger.Guard("App.RevealInFileManager", &err)

	// #nosec G204 -- reveals the user's own local flow file in the OS file
	// manager; the path originates from this desktop session, not remote input.
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", "/select,", path) // #nosec G204
	case "darwin":
		cmd = exec.Command("open", "-R", path) // #nosec G204
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(path)) // #nosec G204
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap the child; exit status is irrelevant
	return nil
}

func (s *FlowService) SearchFlow(doc *models.FlowDocument, query models.SearchQuery) (results *models.SearchResults, err error) {
	defer logger.Guard("App.SearchFlow", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	idx := s.searchIndexFor(doc)
	return idx.Search(query), nil
}

func (s *FlowService) searchIndexFor(doc *models.FlowDocument) *search.SearchIndex {
	if v, ok := s.idxCache.Get(context.Background(), doc.ID); ok {
		return v.(*search.SearchIndex)
	}
	idx := search.NewSearchIndex(doc.ID, doc)
	s.idxCache.Set(context.Background(), doc.ID, idx, 0)
	return idx
}

func (s *FlowService) GetSourceFiles(doc *models.FlowDocument) (result []models.SourceFileInfo, err error) {
	defer logger.Guard("App.GetSourceFiles", &err)

	if doc == nil {
		return nil, nil
	}

	seen := make(map[string]bool)
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		filename := sf.SourceFile
		if filename == "" {
			if doc.FilePath != "" {
				filename = filepath.Base(doc.FilePath)
			} else {
				filename = sf.Name + ".txt"
			}
		}
		if seen[filename] {
			continue
		}
		seen[filename] = true

		lineCount := 0
		for _, b := range sf.Blocks {
			if b.LineNumber > lineCount {
				lineCount = b.LineNumber
			}
		}

		result = append(result, models.SourceFileInfo{
			Filename:    filename,
			SubflowID:   sf.ID,
			SubflowName: sf.Name,
			BlockCount:  len(sf.Blocks),
			LineCount:   lineCount,
		})
	}

	return result, nil
}

func (s *FlowService) ReadSourceFiles(doc *models.FlowDocument, filenames []string) (result map[string]string, err error) {
	defer logger.Guard("App.ReadSourceFiles", &err)

	if doc == nil {
		return nil, nil
	}

	result = make(map[string]string, len(filenames))
	dir := filepath.Dir(doc.FilePath)

	for _, name := range filenames {
		// Reject empty/null-byte names before touching the filesystem, matching
		// the guard the other path-accepting methods (LoadFlowFromPath,
		// LoadFlowFolder) already apply. Defense-in-depth: this endpoint is
		// local-mode only, but a misbehaving frontend shouldn't reach os.ReadFile
		// with malformed input.
		if err := validateUserPath(name); err != nil {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path) // #nosec G304 -- reading subflow files from the opened flow folder
		if err != nil {
			if doc.FilePath != "" && filepath.Base(doc.FilePath) == name {
				data, err2 := os.ReadFile(doc.FilePath)
				if err2 == nil {
					result[name] = string(data)
				}
			}
			continue
		}
		result[name] = string(data)
	}

	return result, nil
}

func (s *FlowService) OnFileOpenFromSystem(path string) {
	if path == "" {
		return
	}
	path = strings.TrimSpace(path)
	if _, err := os.Stat(path); err != nil {
		logger.Warn("file open from system: file not found", "path", path)
		return
	}
	doc, err := s.loadAndParse(path)
	if err != nil {
		logger.Error("file open from system failed", "path", path, "error", err)
		s.notifier.Emit("flow:load-error", map[string]any{
			"path": path, "error": err.Error(),
		})
		return
	}
	s.notifier.Emit("flow:loaded", doc)
}

// loadAndParse reads a file, parses it, updates document and search index state,
// emits events, and adds the file to recents.
func (s *FlowService) loadAndParse(path string) (*models.FlowDocument, error) {
	if s.settings == nil {
		return nil, fmt.Errorf("application not fully initialized")
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("file not found: %w", err)
	}

	maxSizeMB := s.settings.Get().Parser.MaxFileSizeMB
	maxSize := int64(maxSizeMB) * 1024 * 1024
	if maxSize > 0 && info.Size() > maxSize {
		return nil, fmt.Errorf("file too large (%d MB). Max is %d MB", info.Size()/(1024*1024), maxSizeMB)
	}

	data, err := os.ReadFile(path) // #nosec G304 -- reading a flow file the app was asked to open
	if err != nil {
		return nil, fmt.Errorf("couldn't read file: %w", err)
	}

	hash := sha256.Sum256(data)
	key := "ast:" + hex.EncodeToString(hash[:])

	if s.astCache != nil {
		if cached, ok := s.astCache.Get(context.Background(), key); ok {
			if doc, ok := cached.(*models.FlowDocument); ok {
				// Shallow-copy before mutating FilePath so the cached
				// original stays pristine for concurrent callers.
				docCopy := *doc
				docCopy.FilePath = path
				doc = &docCopy
				s.notifier.Emit("flow:loaded", doc)
				logger.Info("flow loaded from cache", "file", filepath.Base(path))
				return doc, nil
			}
		}
	}

	text := string(data)
	fileName := filepath.Base(path)

	var doc *models.FlowDocument
	if info.Size() > 1_000_000 {
		doc, err = parser.ParseTextWithProgress(text, fileName, info.Size(), func(percent int, message string) {
			s.notifier.Emit("flow:parse-progress", map[string]any{
				"percent": percent, "message": message,
			})
		})
	} else {
		doc, err = parser.ParseText(text, fileName, info.Size())
	}
	if err != nil {
		return nil, fmt.Errorf("parse failed: %w", err)
	}

	doc.FilePath = path

	if s.astCache != nil {
		s.astCache.Set(context.Background(), key, doc, 24*time.Hour)
	}

	if s.settings != nil {
		_ = s.settings.AddRecentFile(path, info.Size())
	}

	s.notifier.Emit("flow:loaded", doc)

	logger.Info("flow parsed",
		"file", fileName,
		"subflows", doc.Metadata.SubflowCount,
		"blocks", doc.Metadata.BlockCount,
	)

	return doc, nil
}

func searchBlock(blocks []models.Block, id string) *models.Block {
	for i := range blocks {
		if blocks[i].ID == id {
			return &blocks[i]
		}
		if len(blocks[i].Children) > 0 {
			if found := searchBlock(blocks[i].Children, id); found != nil {
				return found
			}
		}
	}
	return nil
}

// PatchFlow applies a textual Patch to the flow's source file on disk, writes it
// back atomically, and re-parses the whole flow so the returned document (and
// the in-memory current doc) reflect the edit. Desktop/local only — cloud flows
// have no on-disk source to patch. The patch edits the RAW source (never a
// re-serialization), preserving PAD's original parameter order/quoting; the
// re-parse validates the edit is structurally sound.
//
// For a multi-file (folder) flow, patch.File selects the subflow file; it must
// be a bare filename (no path separators) to prevent traversal outside the flow
// folder. An empty patch.File on a folder flow targets the block's own subflow
// file (resolved by SuppressFindingInSource) or falls back to Main.
func (s *FlowService) PatchFlow(doc *models.FlowDocument, patch models.Patch) (*models.FlowDocument, error) {
	if doc == nil || doc.FilePath == "" {
		return nil, fmt.Errorf("patching requires a local source file (desktop mode)")
	}
	info, err := os.Stat(doc.FilePath)
	if err != nil {
		return nil, fmt.Errorf("source not accessible: %w", err)
	}

	targetPath := doc.FilePath
	if info.IsDir() {
		fileName := patch.File
		if fileName == "" {
			fileName = "Main.txt"
		}
		if !safeConvComponent(fileName) { // reuse the path-traversal guard
			return nil, fmt.Errorf("invalid patch file %q", fileName)
		}
		targetPath = filepath.Join(doc.FilePath, fileName)
	}

	data, readErr := os.ReadFile(targetPath) // #nosec G304 -- target derived from doc.FilePath + a validated bare filename
	if readErr != nil {
		return nil, fmt.Errorf("read source file: %w", readErr)
	}

	patched := analyzer.ApplyPatch(string(data), patch)
	dir := filepath.Dir(targetPath)
	if writeErr := atomicWriteConv(dir, targetPath, []byte(patched)); writeErr != nil {
		return nil, fmt.Errorf("write source file: %w", writeErr)
	}

	// Re-parse the whole flow so cross-subflow indexes/state are consistent.
	if info.IsDir() {
		return s.LoadFlowFolder(doc.FilePath)
	}
	return s.LoadFlowFromPath(doc.FilePath)
}

// SuppressFindingInSource writes a `# pad-ignore[ruleID]` directive into the
// flow's source file immediately before the given block, then re-parses — so a
// suppression travels with the file (honored by the analyzer, CLI gate,
// baselines, and CI), unlike a UI-only suppression. Returns the re-parsed doc.
// ruleID "" suppresses all rules on that block.
func (s *FlowService) SuppressFindingInSource(doc *models.FlowDocument, blockID, ruleID string) (*models.FlowDocument, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	block := doc.BlocksByID[blockID]
	if block == nil {
		// BlocksByID is transient; fall back to a tree walk.
		for i := range doc.Subflows {
			if b := searchBlock(doc.Subflows[i].Blocks, blockID); b != nil {
				block = b
				break
			}
		}
	}
	if block == nil {
		return nil, fmt.Errorf("block %q not found", blockID)
	}
	patch := analyzer.SuppressFindingPatch(block, ruleID)
	// For a folder flow, target the subflow file the block lives in.
	if doc.FilePath != "" {
		if info, err := os.Stat(doc.FilePath); err == nil && info.IsDir() {
			if sf := doc.BlockSubflow[blockID]; sf != nil && sf.SourceFile != "" {
				patch.File = sf.SourceFile
			}
		}
	}
	return s.PatchFlow(doc, patch)
}

// ApplyFix applies a deterministic auto-fix to a block in the flow's source
// file, then re-parses. fixType selects the fixer:
//   - "wrap-error-handler": wrap the block in ON BLOCK ERROR … END (resolves
//     unhandled-error and file-op-no-error-handler).
//   - "suppress": insert a # pad-ignore[ruleID] directive (same as
//     SuppressFindingInSource).
//
// Returns the re-parsed document. Desktop/local only (PatchFlow needs an
// on-disk source). Unknown fix types return an error.
// generateFixPatch resolves the block and builds the patch for the given fix
// type. Shared by ApplyFix (writes to disk) and PreviewFix (returns text only).
func (s *FlowService) generateFixPatch(doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (models.Patch, error) {
	if doc == nil {
		return models.Patch{}, fmt.Errorf("no flow loaded")
	}
	block := doc.BlocksByID[blockID]
	if block == nil {
		for i := range doc.Subflows {
			if b := searchBlock(doc.Subflows[i].Blocks, blockID); b != nil {
				block = b
				break
			}
		}
	}
	if block == nil {
		return models.Patch{}, fmt.Errorf("block %q not found", blockID)
	}

	var patch models.Patch
	switch fixType {
	case "wrap-error-handler":
		patch = analyzer.WrapInErrorHandlerPatch(block)
	case "insert-close":
		patch = analyzer.InsertClosePatch(block)
	case "set-timeout":
		patch = analyzer.SetTimeoutPatch(block)
	case "insert-delay":
		patch = analyzer.InsertDelayPatch(block)
	case "insert-handler-log":
		patch = analyzer.InsertHandlerLogPatch(block)
	case "init-variable":
		patch = analyzer.InsertVariableInitPatch(block, variable)
	case "insert-error-log":
		patch = analyzer.InsertErrorLogPatch(block)
	case "replace-with-variable":
		patch = analyzer.ReplaceWithVariablePatch(block, property)
	case "wrap-in-retry":
		patch = analyzer.WrapInRetryPatch(block)
	case "insert-exit-condition":
		patch = analyzer.InsertExitConditionPatch(block)
	case "suppress":
		patch = analyzer.SuppressFindingPatch(block, ruleID)
	default:
		return models.Patch{}, fmt.Errorf("unknown fix type %q", fixType)
	}
	if doc.FilePath != "" {
		if info, err := os.Stat(doc.FilePath); err == nil && info.IsDir() {
			if sf := doc.BlockSubflow[blockID]; sf != nil && sf.SourceFile != "" {
				patch.File = sf.SourceFile
			}
		}
	}
	return patch, nil
}

func (s *FlowService) ApplyFix(doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (*models.FlowDocument, error) {
	patch, err := s.generateFixPatch(doc, blockID, fixType, ruleID, variable, property)
	if err != nil {
		return nil, err
	}
	return s.PatchFlow(doc, patch)
}

// PatchPreviewResult holds the before/after source text for a dry-run fix
// preview. The frontend renders a diff so the user can review the change before
// committing it to disk.
type PatchPreviewResult struct {
	Original string `json:"original"`
	Patched  string `json:"patched"`
}

// PreviewFix generates the patch and returns the before/after source text
// WITHOUT writing to disk. Lets the user review the change before applying.
func (s *FlowService) PreviewFix(doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (*PatchPreviewResult, error) {
	patch, err := s.generateFixPatch(doc, blockID, fixType, ruleID, variable, property)
	if err != nil {
		return nil, err
	}

	targetPath := doc.FilePath
	if doc.FilePath != "" {
		if info, err := os.Stat(doc.FilePath); err == nil && info.IsDir() {
			fileName := patch.File
			if fileName == "" {
				fileName = "Main.txt"
			}
			if !safeConvComponent(fileName) {
				return nil, fmt.Errorf("invalid patch file %q", fileName)
			}
			targetPath = filepath.Join(doc.FilePath, fileName)
		}
	}

	data, readErr := os.ReadFile(targetPath) // #nosec G304 -- same path logic as PatchFlow
	if readErr != nil {
		return nil, fmt.Errorf("read source file: %w", readErr)
	}

	original := string(data)
	patched := analyzer.ApplyPatch(original, patch)
	return &PatchPreviewResult{Original: original, Patched: patched}, nil
}
