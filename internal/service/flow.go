package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	notifier    EventNotifier
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

	// patchLocks serializes read-modify-write patches to the same source file so
	// two concurrent ApplyFix/SuppressFindingInSource calls on the same file
	// can't silently clobber each other (one overwriting the other's change).
	// Keyed by absolute target path; each entry is a *sync.Mutex held for the
	// PatchFlow critical section. Without this, PatchFlow's
	// ReadFile→ApplyPatch→atomicWrite window is a classic lost-update race.
	patchLocks sync.Map
}

// maxSearchIndexCache bounds the number of cached search indexes (one per
// distinct flow). Each index is roughly proportional to block count, so this
// caps worst-case memory regardless of how many flows are opened over uptime.
const maxSearchIndexCache = 64

func NewFlowService(notifier EventNotifier, settings SettingsProvider, docProvider DocumentProvider, storage storageif.StorageBackend, authz *AuthzService, astCache cache.Cache) *FlowService {
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

// patchMutexFor returns the per-file mutex used to serialize patches to a given
// source file. LoadOrStore makes the lookup-and-create atomic so two concurrent
// callers get the same mutex instance.
func (s *FlowService) patchMutexFor(path string) *sync.Mutex {
	v, _ := s.patchLocks.LoadOrStore(path, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// GetAuthorized loads a flow and verifies the user has at least minPerm access.
// minPerm is "viewer", "editor", or "admin". All policy lives in AuthzService.
//
// The authz decision is made HEADER-FIRST (CheckFlowAccessByID reads only the
// flow's owner/org metadata) and the document content is resolved only after
// the check passes — a denied caller costs one indexed header query instead
// of a full blob download + unmarshal + index rebuild.
func (s *FlowService) GetAuthorized(ctx context.Context, flowID, userID, minPerm string) (*models.FlowDocument, error) {
	if s.storage == nil { // Local mode
		return s.docProvider.ResolveDoc(ctx, flowID)
	}

	if s.authz != nil {
		if err := s.authz.CheckFlowAccessByID(ctx, flowID, userID, minPerm); err != nil {
			return nil, err
		}
	}

	doc, err := s.docProvider.ResolveDoc(ctx, flowID)
	if err != nil {
		return nil, err
	}
	return doc, nil
}

// CheckFlowPermission verifies access WITHOUT resolving the document content
// — the authorization decision needs only the flow header (owner/org), so
// perm-only handlers (triage, comments, sharing, versions) skip the blob
// download + unmarshal + index rebuild entirely. Local mode always allows.
func (s *FlowService) CheckFlowPermission(ctx context.Context, flowID, userID, minPerm string) error {
	if s.storage == nil { // Local mode
		return nil
	}
	if s.authz == nil {
		return ErrPermissionDenied
	}
	return s.authz.CheckFlowAccessByID(ctx, flowID, userID, minPerm)
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

// FindBlockByID resolves a block by ID, preferring the fast BlocksByID map and
// falling back to a tree walk over doc.Subflows when the map doesn't have it
// (BlocksByID is transient — e.g. rebuilt on reparse — so a caller holding an
// ID from before a reparse still resolves correctly).
func (s *FlowService) FindBlockByID(doc *models.FlowDocument, blockID string) *models.Block {
	if doc == nil || blockID == "" {
		return nil
	}
	if b, ok := doc.BlocksByID[blockID]; ok {
		return b
	}
	for i := range doc.Subflows {
		if b := searchBlock(doc.Subflows[i].Blocks, blockID); b != nil {
			return b
		}
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
				// Shallow-copy so concurrent callers loading identical content
				// don't share the same mutable pointer (matches loadAndParse).
				docCopy := *doc
				doc = &docCopy
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

// SaveUploadedFlow persists a freshly-parsed/uploaded flow document into the
// library (cloud mode only). It applies optimistic concurrency: if a flow with
// this ID already exists, its current version is loaded first (header only —
// content isn't needed) so SaveFlow's OCC check can't silently clobber a
// concurrent edit; a brand-new flow keeps version 0 (insert path). Returns
// storageif.ErrVersionConflict unchanged so the caller can map it to its own
// HTTP status. A nil s.storage (local/desktop mode) is a no-op success.
func (s *FlowService) SaveUploadedFlow(ctx context.Context, doc *models.FlowDocument, ownerID string) error {
	if s.storage == nil {
		return nil
	}
	content, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("failed to marshal uploaded flow: %w", err)
	}
	libDoc := storageif.FlowDocument{
		ID:      doc.ID,
		Name:    doc.Name,
		Content: content,
		Source:  doc.Source, // raw PAD text (single-file); enables cloud-mode fix
		OwnerID: ownerID,
		Metadata: storageif.FlowMetadata{
			BlockCount:   doc.Metadata.BlockCount,
			SubflowCount: doc.Metadata.SubflowCount,
		},
	}
	existing, err := s.storage.LoadFlowHeader(ctx, doc.ID)
	if err == nil && existing != nil {
		libDoc.Version = existing.Version
	} else if err != nil && !errors.Is(err, storageif.ErrNotFound) {
		// Transient DB error — must not silently bypass OCC.
		return fmt.Errorf("failed to check existing flow: %w", err)
	}
	return s.storage.SaveFlow(ctx, &libDoc)
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

// maxLibrarySearchFlows bounds how many stored flows a cross-library search will
// load + index. Each flow is a ResolveDoc (DB read) + index build, so capping
// keeps a single search cheap; the idxCache LRU makes repeat searches on the
// same library near-free. 50 covers a typical personal library comfortably.
const maxLibrarySearchFlows = 50

// SearchLibrary runs a query across every flow the caller can access (RLS-scoped
// by UserID at the storage layer), merging per-flow hits into one result set with
// each hit stamped by its source FlowID/FlowName so the UI can group them. A
// cross-flow search is an enumerate→load→index loop; there is no storage-level
// "search across flows" primitive. Per-flow MaxResults is clamped so one huge
// flow doesn't monopolise the result, then a global cap is applied.
func (s *FlowService) SearchLibrary(ctx context.Context, userID string, query models.SearchQuery) (*models.SearchResults, error) {
	if s.storage == nil || s.docProvider == nil {
		// Local/no storage: nothing stored to search across.
		return &models.SearchResults{Query: query, Results: []models.SearchResult{}}, nil
	}
	// MetadataOnly: the loop below re-resolves each flow's full document via
	// ResolveDoc (it needs the parsed doc, not the raw stored copy); the list
	// phase only uses ID/Name. Without this, the Postgres backend backfills
	// full blob content for up to maxLibrarySearchFlows flows that is then
	// discarded — a wasted blob round-trip per flow per search.
	flows, err := s.storage.ListFlows(ctx, storageif.FlowFilter{UserID: userID, Limit: maxLibrarySearchFlows, MetadataOnly: true})
	if err != nil {
		return nil, fmt.Errorf("list flows for search: %w", err)
	}
	// Clamp per-flow hits so no single flow drowns out the others.
	perFlow := query.MaxResults
	if perFlow <= 0 || perFlow > 10 {
		perFlow = 10
	}
	merged := make([]models.SearchResult, 0)
	for _, f := range flows {
		if ctx.Err() != nil {
			break
		}
		doc, err := s.docProvider.ResolveDoc(ctx, f.ID)
		if err != nil || doc == nil {
			continue
		}
		q := query
		q.MaxResults = perFlow
		res, err := s.SearchFlow(doc, q)
		if err != nil || res == nil {
			continue
		}
		for i := range res.Results {
			res.Results[i].FlowID = f.ID
			res.Results[i].FlowName = f.Name
		}
		merged = append(merged, res.Results...)
	}
	// Global cap (default 50 when unset).
	globalCap := query.MaxResults
	if globalCap <= 0 {
		globalCap = 50
	}
	if len(merged) > globalCap {
		merged = merged[:globalCap]
	}
	return &models.SearchResults{Query: query, Results: merged, TotalCount: len(merged)}, nil
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

	// Serialize read-modify-write patches to this file: without a per-file lock,
	// two concurrent ApplyFix/SuppressFindingInSource calls could each ReadFile
	// the same contents, apply their patch, and the second atomicWrite would
	// silently overwrite the first (lost update). Held through the re-parse so
	// the returned doc reflects exactly what we wrote.
	mu := s.patchMutexFor(targetPath)
	mu.Lock()
	defer mu.Unlock()

	data, readErr := os.ReadFile(targetPath) // #nosec G304 -- target derived from doc.FilePath + a validated bare filename
	if readErr != nil {
		return nil, fmt.Errorf("read source file: %w", readErr)
	}

	patched := analyzer.ApplyPatch(string(data), patch)

	// Validate the patched source before persisting. ApplyPatch is purely
	// textual, so a malformed patch — or a concurrent edit shifting line
	// numbers between the ReadFile above and the write below — could introduce
	// unbalanced block structure. The parser is lenient (it always returns a
	// doc with errors embedded rather than a Go error), so we compare the
	// error-severity parse-error counts: writing first would silently replace
	// valid source with degraded source the user can't undo. Reject only when
	// the patch makes things worse, so fixes on already-imperfect files still
	// land. Two parses total: the before-count parses the bytes JUST READ from
	// disk (authoritative even if the doc in hand predates an external edit),
	// and the after-parse doubles as the returned document below.
	fileName := filepath.Base(targetPath)
	beforeDoc, beforeErr := parser.ParseText(string(data), fileName, int64(len(data)))
	before := 1 << 30
	if beforeErr == nil && beforeDoc != nil {
		before = analyzer.CountParseErrors(beforeDoc)
	}
	afterDoc, afterErr := parser.ParseText(patched, fileName, int64(len(patched)))
	if afterErr != nil || analyzer.CountParseErrors(afterDoc) > before {
		return nil, fmt.Errorf("patch would introduce parse errors (file left unchanged)")
	}

	dir := filepath.Dir(targetPath)
	if writeErr := atomicWriteConv(dir, targetPath, []byte(patched)); writeErr != nil {
		return nil, fmt.Errorf("write source file: %w", writeErr)
	}

	// Folder flows must re-parse the whole folder (cross-subflow indexes);
	// single-file flows reuse the gate's after-parse — it is the parse of
	// exactly the bytes just written, so a reload would only repeat it.
	if info.IsDir() {
		return s.LoadFlowFolder(doc.FilePath)
	}
	afterDoc.FilePath = doc.FilePath
	if s.astCache != nil {
		hash := sha256.Sum256([]byte(patched))
		s.astCache.Set(context.Background(), "ast:"+hex.EncodeToString(hash[:]), afterDoc, 24*time.Hour)
	}
	if s.settings != nil {
		_ = s.settings.AddRecentFile(doc.FilePath, int64(len(patched)))
	}
	s.notifier.Emit("flow:loaded", afterDoc)
	s.docProvider.SetCurrentDoc(afterDoc)
	return afterDoc, nil
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
	block := s.FindBlockByID(doc, blockID)
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
	block := s.FindBlockByID(doc, blockID)
	if block == nil {
		return models.Patch{}, fmt.Errorf("block %q not found", blockID)
	}

	patch, err := analyzer.PatchForFix(block, fixType, ruleID, variable, property)
	if err != nil {
		return models.Patch{}, err
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

func (s *FlowService) ApplyFix(ctx context.Context, doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (*models.FlowDocument, error) {
	patch, err := s.generateFixPatch(doc, blockID, fixType, ruleID, variable, property)
	if err != nil {
		return nil, err
	}
	// Cloud mode (no local source file): patch the in-memory raw source and
	// persist via SaveFlow. Desktop mode falls through to PatchFlow (file I/O).
	if doc.FilePath == "" {
		return s.applyPatchToCloudSource(ctx, doc, patch)
	}
	return s.PatchFlow(doc, patch)
}

// applyPatchToCloudSource patches the stored raw PAD source (cloud mode),
// validates the result (no new parse errors), re-parses, and persists the
// updated content + source via SaveFlow. For single-file flows, patches the
// doc.Source directly. For multi-file flows, doc.Source contains the combined
// applyPatchToCloudSource patches the stored raw PAD source (cloud mode),
// validates the result (no new parse errors), re-parses, and persists the
// updated content + source via SaveFlow. Only works for single-file flows
// (multi-file flows don't have stored per-file source — cloud fix is deferred).
func (s *FlowService) applyPatchToCloudSource(ctx context.Context, doc *models.FlowDocument, patch models.Patch) (*models.FlowDocument, error) {
	if doc.Source == "" {
		return nil, fmt.Errorf("source not available — cloud fix requires a single-file flow (multi-file cloud fix is not yet supported)")
	}
	patched := analyzer.ApplyPatch(doc.Source, patch)
	// doc.ParseErrors is the parse of doc.Source itself (Content and Source
	// are persisted together), so it serves as the before-count without a
	// second full parse of the unpatched source.
	updated, err := parser.ParseText(patched, doc.Name, int64(len(patched)))
	if err != nil {
		return nil, fmt.Errorf("re-parse patched source: %w", err)
	}
	if analyzer.CountParseErrors(updated) > analyzer.CountParseErrors(doc) {
		return nil, fmt.Errorf("patch would introduce parse errors (flow left unchanged)")
	}
	return s.persistCloudDoc(ctx, doc, updated, patched)
}

// GetSource returns the raw PAD source text for a flow. Desktop: reads the
// source file. Cloud: returns doc.Source. Used by the in-app source editor.
func (s *FlowService) GetSource(doc *models.FlowDocument) (string, error) {
	if doc.FilePath == "" {
		return doc.Source, nil
	}
	targetPath := doc.FilePath
	if info, err := os.Stat(doc.FilePath); err == nil && info.IsDir() {
		targetPath = filepath.Join(doc.FilePath, "Main.txt")
	}
	data, err := os.ReadFile(targetPath) // #nosec G304 -- same path logic as PatchFlow
	if err != nil {
		return "", fmt.Errorf("read source file: %w", err)
	}
	return string(data), nil
}

// SaveSource replaces the flow's raw source text with the given source (from
// the in-app source editor), re-parses, and persists. Desktop: writes the file
// then re-reads/re-parses. Cloud: re-parses the source into Content + Source
// and saves via SaveFlow (OCC). Used by the "Save & Re-parse" button in the
// source editor view.
func (s *FlowService) SaveSource(ctx context.Context, doc *models.FlowDocument, source string) (*models.FlowDocument, error) {
	if doc.FilePath == "" {
		// Cloud: persist via the shared cloud-source path.
		return s.persistCloudSource(ctx, doc, source)
	}
	// Desktop: write the source to the file, then re-parse the whole flow.
	targetPath := doc.FilePath
	if info, err := os.Stat(doc.FilePath); err == nil && info.IsDir() {
		targetPath = filepath.Join(doc.FilePath, "Main.txt")
	}
	if err := atomicWriteConv(filepath.Dir(targetPath), targetPath, []byte(source)); err != nil {
		return nil, fmt.Errorf("write source file: %w", err)
	}
	if info, _ := os.Stat(doc.FilePath); info != nil && info.IsDir() {
		return s.LoadFlowFolder(doc.FilePath)
	}
	return s.LoadFlowFromPath(doc.FilePath)
}

// ApplyFixBatch applies multiple auto-fixes in one pass (the iterative loop in
// analyzer.ApplyFixesToSource), persisting once at the end. ruleFilter (nil ⇒
// all auto-fixable rules) selects which rules to fix — the bulk-action bar
// derives it from the selected findings' rules. Returns the re-parsed doc and
// the number of fixes applied. Works in desktop (file) and cloud (stored
// source) modes for single-file flows.
func (s *FlowService) ApplyFixBatch(ctx context.Context, doc *models.FlowDocument, ruleFilter map[string]bool, limit int) (*models.FlowDocument, int, error) {
	if limit <= 0 {
		limit = 100 // safety cap matching the CLI default band
	}

	// Resolve the source text to patch.
	var source, targetPath string
	cloud := doc.FilePath == ""
	if cloud {
		if doc.Source == "" {
			return nil, 0, fmt.Errorf("source not available for this flow (batch fix requires a single-file flow)")
		}
		source = doc.Source
	} else {
		targetPath = doc.FilePath
		if info, err := os.Stat(doc.FilePath); err == nil && info.IsDir() {
			targetPath = filepath.Join(doc.FilePath, "Main.txt")
		}
		data, err := os.ReadFile(targetPath) // #nosec G304 -- target derived from doc.FilePath
		if err != nil {
			return nil, 0, fmt.Errorf("read source file: %w", err)
		}
		source = string(data)
	}

	// One loop pass replaces four parses: the loop itself parses the original
	// (first iteration) and the fixed source (last iteration), and reports both
	// error counts — no separate before/after parseErrorCount calls needed.
	res, err := analyzer.ApplyFixesToSourceDoc(&source, doc.Name, ruleFilter, limit, nil)
	if err != nil {
		return nil, res.Fixed, err
	}
	if res.Fixed == 0 {
		return doc, 0, nil // nothing changed
	}
	// Same gate as PatchFlow: a batch must not leave the source worse than before.
	if res.AfterErrors > res.BeforeErrors {
		return nil, res.Fixed, fmt.Errorf("batch fix would introduce parse errors (flow left unchanged)")
	}

	if cloud {
		// res.Doc is already the parse of the patched source — persist it
		// directly instead of re-parsing inside persistCloudSource.
		updated, err := s.persistCloudDoc(ctx, doc, res.Doc, source)
		return updated, res.Fixed, err
	}
	// Desktop: write once, then re-parse the whole flow (cross-subflow indexes).
	if err := atomicWriteConv(filepath.Dir(targetPath), targetPath, []byte(source)); err != nil {
		return nil, res.Fixed, fmt.Errorf("write source file: %w", err)
	}
	if info, _ := os.Stat(doc.FilePath); info != nil && info.IsDir() {
		updated, err := s.LoadFlowFolder(doc.FilePath)
		return updated, res.Fixed, err
	}
	// Seed the AST cache with the loop's parse of the exact bytes just
	// written, so the reload below is a cache hit (read + hash) instead of
	// another full parse of content we already parsed.
	if s.astCache != nil {
		hash := sha256.Sum256([]byte(source))
		s.astCache.Set(ctx, "ast:"+hex.EncodeToString(hash[:]), res.Doc, 24*time.Hour)
	}
	updated, err := s.LoadFlowFromPath(doc.FilePath)
	if err != nil {
		return nil, res.Fixed, err
	}
	return updated, res.Fixed, nil
}

// persistCloudDoc stamps an ALREADY-PARSED doc with the original's cloud
// identity and saves it (content + source) via SaveFlow with an OCC version
// check. `updated` must be the parse of `source` — callers that have the parse
// in hand (apply-fix paths) use this directly; everyone else uses
// persistCloudSource, which parses first.
func (s *FlowService) persistCloudDoc(ctx context.Context, doc *models.FlowDocument, updated *models.FlowDocument, source string) (*models.FlowDocument, error) {
	updated.ID = doc.ID
	updated.OwnerID = doc.OwnerID
	updated.OrganizationID = doc.OrganizationID
	updated.Source = source
	updated.RebuildIndexes()
	// Persist only when a storage backend is configured (cloud). Local mode
	// never reaches here (doc.FilePath is set locally).
	if s.storage != nil {
		content, err := json.Marshal(updated)
		if err != nil {
			return nil, fmt.Errorf("marshal patched flow: %w", err)
		}
		libDoc := storageif.FlowDocument{
			ID:             doc.ID,
			Name:           doc.Name,
			Content:        content,
			Source:         updated.Source, // empty for multi-file flows
			OwnerID:        doc.OwnerID,
			OrganizationID: doc.OrganizationID,
			Metadata: storageif.FlowMetadata{
				BlockCount:   updated.Metadata.BlockCount,
				SubflowCount: updated.Metadata.SubflowCount,
			},
		}
		// OCC: load the current version so SaveFlow can detect a concurrent edit.
		if header, hErr := s.storage.LoadFlowHeader(ctx, doc.ID); hErr == nil && header != nil {
			libDoc.Version = header.Version
			libDoc.Description = header.Description
		} else if hErr != nil && !errors.Is(hErr, storageif.ErrNotFound) {
			return nil, fmt.Errorf("check existing flow: %w", hErr)
		}
		if err := s.storage.SaveFlow(ctx, &libDoc); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// persistCloudSource re-parses the patched source and saves it (content +
// source) via SaveFlow with an OCC version check. Shared by the source editor
// save and the batch apply-fix (cloud) paths.
func (s *FlowService) persistCloudSource(ctx context.Context, doc *models.FlowDocument, source string) (*models.FlowDocument, error) {
	updated, err := parser.ParseText(source, doc.Name, int64(len(source)))
	if err != nil {
		return nil, fmt.Errorf("re-parse patched source: %w", err)
	}
	return s.persistCloudDoc(ctx, doc, updated, source)
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

	// Cloud mode (no local source file): patch the in-memory raw source.
	if doc.FilePath == "" {
		if doc.Source == "" {
			return nil, fmt.Errorf("source not available for this flow (cloud preview requires a single-file flow)")
		}
		patched := analyzer.ApplyPatch(doc.Source, patch)
		return &PatchPreviewResult{Original: doc.Source, Patched: patched}, nil
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

// parseErrorCount parses source and returns the number of error-severity (not
// warning) parse problems. The parser is lenient — it always returns a doc —
// so this is the only way to detect structurally broken PAD. Used by PatchFlow
// to guard against persisting a patch that introduces block-structure errors.
func parseErrorCount(source, fileName string) int {
	doc, err := parser.ParseText(source, fileName, int64(len(source)))
	if err != nil || doc == nil {
		// ParseText never returns a hard error today, but treat a future hard
		// failure as "maximally broken".
		return 1 << 30
	}
	return analyzer.CountParseErrors(doc)
}
