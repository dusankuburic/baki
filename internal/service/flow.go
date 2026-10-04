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
	"pad-analyzer/internal/metrics"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"weak"

	"golang.org/x/sync/singleflight"
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
	// uploadIDs (cloud) remembers the upload's minted flow id, keyed like the AST cache so a re-upload updates that flow.
	uploadIDs cache.Cache

	// idxCache memoises search indexes by flow ID, each valid only for the document object it was built from.
	idxCache cache.Cache

	// profiles resolves the per-org rule profile so "fix all" cannot apply a rule the org disabled; nil in desktop/tests.
	profiles *RuleProfileResolver

	// snapshots is the in-memory undo ring (flow_snapshots.go); nil when disabled.
	snapshots *snapshotStore

	// invalidateCbs holds invalidation callbacks from other services — indirect wiring avoids a DI cycle.
	invalidateMu  sync.Mutex
	invalidateCbs []func(flowID string)

	// onCloudFlowMutation fires after cloud flow writes other services' user-keyed caches key on; DI setter avoids a constructor cycle.
	onCloudFlowMutation func(ownerID string)

	// idxFlights dedups concurrent index builds — search-as-you-type would otherwise rebuild per keystroke.
	idxFlights singleflight.Group
	// patchLocks serializes read-modify-write patches per absolute path; entries drop at zero users.
	patchLocksMu sync.Mutex
	patchLocks   map[string]*patchFileLock
}

// The search-index cache is bounded by bytes as well as count; see searchIndexCost.
const (
	maxSearchIndexCache          = 64
	searchIndexCacheBudget int64 = 48 << 20
	// searchIndexCostPerBlock: 2× the measured ~240 B/block, so the budget holds on token-dense flows.
	searchIndexCostPerBlock int64 = 512
)

func searchIndexCost(idx *search.SearchIndex) int64 {
	blocks := int64(idx.BlockCount())
	if blocks < 1 {
		blocks = 1
	}
	return blocks * searchIndexCostPerBlock
}

// FlowLoadedSummary is the SSE payload for "flow:loaded" — identity/refetch keys only, since carrying the whole document pinned gigabytes across stalled SSE connections; consumers refetch over HTTP.
type FlowLoadedSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// FilePath is the local-mode refetch key; empty for cloud flows (refetched from the library).
	FilePath string `json:"filePath,omitempty"`
	// IsFolder selects load-folder over load-path on the refetch.
	IsFolder bool `json:"isFolder,omitempty"`
}

// FlowLoadedSummaryOf builds the lightweight "flow:loaded" payload for doc.
func FlowLoadedSummaryOf(doc *models.FlowDocument) FlowLoadedSummary {
	if doc == nil {
		return FlowLoadedSummary{}
	}
	return FlowLoadedSummary{ID: doc.ID, Name: doc.Name, FilePath: doc.FilePath, IsFolder: doc.IsFolder}
}

func NewFlowService(notifier EventNotifier, settings SettingsProvider, docProvider DocumentProvider, storage storageif.StorageBackend, authz *AuthzService, astCache cache.Cache) *FlowService {
	idxCache := cache.NewSizedLRU(searchIndexCacheBudget, cache.WithMaxEntries(maxSearchIndexCache))
	snapshotBudget := maxSnapshotBytes
	if storage != nil {
		// Cloud keeps no AST cache: the stored library is the source of truth; the undo ring is container-budgeted.
		astCache = nil
		snapshotBudget = maxSnapshotBytesCloud
	}
	svc := &FlowService{
		notifier:    notifier,
		settings:    settings,
		docProvider: docProvider,
		storage:     storage,
		authz:       authz,
		astCache:    astCache,
		idxCache:    idxCache,
		snapshots:   newSnapshotStoreWithBudget(snapshotBudget),
	}
	if storage != nil {
		svc.uploadIDs, _ = cache.NewLRUCache(uploadIDMemoSize) // size > 0 ⇒ error impossible
	}
	return svc
}

const uploadIDMemoSize = 1024

// SetRuleProfileResolver injects the per-org rule resolver; wired by DI after construction (the resolver needs the storage backend from the same graph).
func (s *FlowService) SetRuleProfileResolver(r *RuleProfileResolver) {
	s.profiles = r
}

// SetCloudFlowMutationHook registers fn invoked (best-effort, after the write committed) on cloud flow uploads/saves/patches; a setter to avoid a constructor cycle.
func (s *FlowService) SetCloudFlowMutationHook(fn func(ownerID string)) {
	s.onCloudFlowMutation = fn
}

func (s *FlowService) notifyCloudFlowMutation(ownerID string) {
	if fn := s.onCloudFlowMutation; fn != nil && ownerID != "" {
		fn(ownerID)
	}
}

// fixOptions builds the fix-loop config from the same per-org profile as analysis, so "fix all" can never fix a disabled rule.
func (s *FlowService) fixOptions(ctx context.Context, doc *models.FlowDocument, ruleFilter map[string]bool, limit int) analyzer.FixOptions {
	opts := analyzer.FixOptions{RuleFilter: ruleFilter, Limit: limit}
	if s.profiles != nil {
		profile := s.profiles.Resolve(ctx, doc.OrganizationID)
		opts.Settings, opts.Rules = profile.Settings, profile.Rules
		return opts
	}
	// No resolver (desktop/local, tests): the deployment settings are the
	// whole configuration; nil Settings reads as the built-in defaults.
	if s.settings != nil {
		opts.Settings = s.settings.Get()
	}
	return opts
}

type patchFileLock struct {
	mu   sync.Mutex
	refs int
}

// Map bookkeeping stays under patchLocksMu so an acquire can't interleave a delete.
func (s *FlowService) acquirePatchLock(path string) *patchFileLock {
	s.patchLocksMu.Lock()
	defer s.patchLocksMu.Unlock()
	if s.patchLocks == nil {
		s.patchLocks = map[string]*patchFileLock{}
	}
	l := s.patchLocks[path]
	if l == nil {
		l = &patchFileLock{}
		s.patchLocks[path] = l
	}
	l.refs++
	return l
}

func (s *FlowService) releasePatchLock(path string, l *patchFileLock) {
	s.patchLocksMu.Lock()
	defer s.patchLocksMu.Unlock()
	l.refs--
	if l.refs == 0 && s.patchLocks[path] == l {
		delete(s.patchLocks, path)
	}
}

// GetAuthorized loads a flow after verifying minPerm access ("viewer"/"editor"/"admin"); the authz decision is HEADER-FIRST so a denied caller costs one indexed query, not a blob download.
func (s *FlowService) GetAuthorized(ctx context.Context, flowID, userID, minPerm string) (*models.FlowDocument, error) {
	if s.storage == nil { // Local mode
		return s.docProvider.ResolveDoc(ctx, flowID)
	}

	if s.authz != nil {
		header, err := s.authz.AuthorizeFlowHeader(ctx, flowID, userID, minPerm)
		if err != nil {
			return nil, err
		}
		// One header read serves both the authz check and the doc cache's version check.
		if hr, ok := s.docProvider.(headerDocResolver); ok {
			return hr.ResolveDocWithHeader(ctx, flowID, header)
		}
	}

	doc, err := s.docProvider.ResolveDoc(ctx, flowID)
	if err != nil {
		return nil, err
	}
	return doc, nil
}

type headerDocResolver interface {
	ResolveDocWithHeader(ctx context.Context, id string, header *storageif.FlowDocument) (*models.FlowDocument, error)
}

// GetAuthorizedHeader verifies access like GetAuthorized but returns only the header (no content/source) for write-gating callers; local mode stands the in-memory document in.
func (s *FlowService) GetAuthorizedHeader(ctx context.Context, flowID, userID, minPerm string) (*storageif.FlowDocument, error) {
	if s.storage == nil { // Local mode
		doc, err := s.docProvider.ResolveDoc(ctx, flowID)
		if err != nil {
			return nil, err
		}
		return &storageif.FlowDocument{ID: doc.ID, Name: doc.Name, OwnerID: doc.OwnerID, OrganizationID: doc.OrganizationID}, nil
	}
	if s.authz == nil {
		// Cloud storage without an authorizer is a wiring error; fail closed like CheckFlowPermission.
		return nil, ErrPermissionDenied
	}
	return s.authz.AuthorizeFlowHeader(ctx, flowID, userID, minPerm)
}

// CheckFlowPermission verifies access WITHOUT resolving document content — perm-only handlers skip the blob download; local mode always allows.
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
	// Snapshot the callbacks under the lock so one may register another without deadlock.
	s.invalidateMu.Lock()
	cbs := append([]func(string){}, s.invalidateCbs...)
	s.invalidateMu.Unlock()
	for _, cb := range cbs {
		cb(flowID)
	}
}

// OnInvalidateFlow registers a callback invoked from InvalidateSearchIndex when derived caches must drop.
func (s *FlowService) OnInvalidateFlow(cb func(flowID string)) {
	s.invalidateMu.Lock()
	defer s.invalidateMu.Unlock()
	s.invalidateCbs = append(s.invalidateCbs, cb)
}

// FindBlockByID resolves a block by ID via the BlocksByID map, then a tree walk (the map is transient).
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

	doc, err = parser.ParseFolderWith(folderPath, s.parseFolderMember)
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

// folderMemberCacheKey hashes NAME and bytes (the parse depends on the name).
func folderMemberCacheKey(name, text string) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return "ast:" + hex.EncodeToString(h.Sum(nil))
}

// parseFolderMember memoises member parses by name+content; the cached doc is shared, never mutated.
func (s *FlowService) parseFolderMember(text, name string, size int64) (*models.FlowDocument, error) {
	if s.astCache == nil {
		return parser.ParseText(text, name, size)
	}
	key := folderMemberCacheKey(name, text)
	if cached, ok := s.astCache.Get(context.Background(), key); ok {
		if doc, ok := cached.(*models.FlowDocument); ok {
			return doc, nil
		}
	}
	doc, err := parser.ParseText(text, name, size)
	if err != nil {
		return nil, err
	}
	s.astCache.Set(context.Background(), key, doc, 24*time.Hour)
	return doc, nil
}

// cacheFolderMember seeds the member cache so a writer that parsed what it wrote skips the re-parse on reload.
func (s *FlowService) cacheFolderMember(name, text string, doc *models.FlowDocument) {
	if s.astCache == nil || doc == nil {
		return
	}
	s.astCache.Set(context.Background(), folderMemberCacheKey(name, text), doc, 24*time.Hour)
}

// LoadAllFromFolder parses every .txt flow export in folderPath; unreadable or unparseable files are skipped but reported in loadErrors (filename → reason).
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
		// On-disk identity: session analytics key on FilePath (StableFlowID),
		// so re-analyzing updates one entry instead of double-counting.
		doc.FilePath = filePath

		// The parser wraps loose content in an implicit subflow, so emptiness is zero blocks, not zero subflows.
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

// LoadFlowFiles parses an upload, memoised keyed by authenticated callerID + content (content-only keys would let users swap flow IDs).
func (s *FlowService) LoadFlowFiles(ctx context.Context, files map[string]string, rootName, callerID string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFiles", &err)

	h := sha256.New()
	// Caller identity first, length-prefixed so a chosen user id can't be confused with content.
	h.Write([]byte(strconv.Itoa(len(callerID))))
	h.Write([]byte(":"))
	h.Write([]byte(callerID))
	// Sort keys for a deterministic hash (Go's map iteration order is random).
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
				// EmitTo, not Emit: reachable from the cloud upload route, where
				// Emit would broadcast to every connected SSE client.
				s.notifier.EmitTo(callerID, "flow:loaded", FlowLoadedSummaryOf(doc))
				logger.Info("flow files loaded from cache", "root", rootName)
				return doc, nil
			}
		}
	}

	doc, err = parser.ParseFiles(files, rootName)
	if err != nil {
		return nil, err
	}

	if s.uploadIDs != nil {
		// Cloud: an identical re-upload keeps its flow id; only ids whose flow still exists are reused.
		if prev, ok := s.uploadIDs.Get(ctx, key); ok {
			if id, ok := prev.(string); ok && id != "" {
				if _, herr := s.storage.LoadFlowHeader(ctx, id); herr == nil {
					doc.ID = id
				} else {
					s.uploadIDs.Delete(ctx, key)
				}
			}
		}
		s.uploadIDs.Set(ctx, key, doc.ID, 24*time.Hour)
	}

	if s.astCache != nil {
		s.astCache.Set(ctx, key, doc, 24*time.Hour)
		// Hand back a SHALLOW COPY: the cache's contract is shared read-only,
		// but the upload handler legitimately stamps doc.Source after load.
		copied := *doc
		doc = &copied
	}

	s.docProvider.SetCurrentDoc(doc)
	s.notifier.EmitTo(callerID, "flow:loaded", FlowLoadedSummaryOf(doc))
	return doc, nil
}

// SaveUploadedFlow persists an uploaded flow (cloud only) with OCC so a concurrent edit can't be clobbered.
func (s *FlowService) SaveUploadedFlow(ctx context.Context, doc *models.FlowDocument, ownerID string) error {
	if s.storage == nil {
		return nil
	}
	content, err := storedFlowContent(doc)
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
	if err := s.storage.SaveFlow(ctx, &libDoc); err != nil {
		return err
	}
	s.notifyCloudFlowMutation(ownerID)
	return nil
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

	return s.searchDoc(doc, query, true), nil
}

// searchDoc runs query over doc; cacheIndex caches only for a doc that outlives the request.
func (s *FlowService) searchDoc(doc *models.FlowDocument, query models.SearchQuery, cacheIndex bool) *models.SearchResults {
	if !cacheIndex || !s.indexReusable(doc) {
		return search.NewSearchIndex(doc.ID, doc).Search(doc, query)
	}
	return s.searchIndexFor(doc).Search(doc, query)
}

// indexReusable: uncached cloud docs above cloudDocCacheMaxEntry are new objects per request; a cached index would be dead on arrival.
func (s *FlowService) indexReusable(doc *models.FlowDocument) bool {
	return s.storage == nil || FlowDocumentCost(doc) <= cloudDocCacheMaxEntry
}

// searchIndexEntry pairs a cached index with a weak ref to the exact doc it was built from.
type searchIndexEntry struct {
	doc weak.Pointer[models.FlowDocument]
	idx *search.SearchIndex
}

// searchIndexFor returns doc's index; reused only for the SAME doc object — a version change rebuilds.
func (s *FlowService) searchIndexFor(doc *models.FlowDocument) *search.SearchIndex {
	if v, ok := s.idxCache.Get(context.Background(), doc.ID); ok {
		if e, ok := v.(*searchIndexEntry); ok && e.doc.Value() == doc {
			return e.idx
		}
	}
	v, _, _ := s.idxFlights.Do(doc.ID, func() (any, error) {
		// Re-check inside the flight: a waiter's doc may differ from the winner's.
		if v, ok := s.idxCache.Get(context.Background(), doc.ID); ok {
			if e, ok := v.(*searchIndexEntry); ok && e.doc.Value() == doc {
				return e.idx, nil
			}
		}
		idx := search.NewSearchIndex(doc.ID, doc)
		entry := &searchIndexEntry{doc: weak.Make(doc), idx: idx}
		// Charged by block count; a count-only cache (a test's) just counts the entry.
		if sized, ok := s.idxCache.(cache.SizedCache); ok {
			sized.SetWithCost(context.Background(), doc.ID, entry, searchIndexCost(idx), 0)
		} else {
			s.idxCache.Set(context.Background(), doc.ID, entry, 0)
		}
		return idx, nil
	})
	return v.(*search.SearchIndex)
}

// maxLibrarySearchFlows bounds stored flows a library search loads; the idxCache LRU makes repeats near-free.
const maxLibrarySearchFlows = 50

// searchFanOut hides blob latency while keeping in-flight documents a small memory fraction.
const searchFanOut = 4

// maxLibrarySearchCandidates caps the pushdown's candidate list (most-recent-first, so the cut keeps live ones).
const maxLibrarySearchCandidates = 50

type uncachedResolver interface {
	ResolveDocUncached(ctx context.Context, id string) (doc *models.FlowDocument, cached bool, err error)
}

// resolveSearchCandidate resolves a candidate WITHOUT caching (no eviction of hot docs).
func (s *FlowService) resolveSearchCandidate(ctx context.Context, id string) (doc *models.FlowDocument, cached bool, err error) {
	if r, ok := s.docProvider.(uncachedResolver); ok {
		return r.ResolveDocUncached(ctx, id)
	}
	doc, err = s.docProvider.ResolveDoc(ctx, id)
	return doc, true, err
}

// SearchLibrary queries every RLS-accessible flow, merging per-flow hits (clamped, then globally capped).
func (s *FlowService) SearchLibrary(ctx context.Context, userID string, query models.SearchQuery) (*models.SearchResults, error) {
	if s.storage == nil || s.docProvider == nil {
		// Local/no storage: HTTP answers 503 before here; this keeps direct callers nil-safe.
		return &models.SearchResults{Query: query, Results: []models.SearchResult{}}, nil
	}
	// Content pushdown (R3-5a): matches come from the index so a search sees the whole library; unsupported backends scan.
	var flows []*storageif.FlowDocument
	candidates, cerr := s.storage.SearchFlowContents(ctx, storageif.FlowFilter{UserID: userID}, query.Text, maxLibrarySearchCandidates)
	switch {
	case cerr == nil && len(candidates) > 0:
		metrics.RecordLibrarySearch("pushdown")
		flows = candidates
	case cerr == nil:
		// Pushdown matched nothing: the library genuinely has no content hit.
		metrics.RecordLibrarySearch("pushdown")
		return &models.SearchResults{Query: query, Results: []models.SearchResult{}, TotalCount: 0}, nil
	default:
		if !errors.Is(cerr, storageif.ErrContentSearchUnsupported) {
			// A real pushdown error degrades to scan rather than failing the search.
			logger.Warn("library search pushdown failed; falling back to scan", "error", cerr)
		}
		metrics.RecordLibrarySearch("scan")
		scanFlows, slErr := s.storage.ListFlows(ctx, storageif.FlowFilter{UserID: userID, Limit: maxLibrarySearchFlows, MetadataOnly: true})
		if slErr != nil {
			return nil, fmt.Errorf("list flows for search: %w", slErr)
		}
		flows = scanFlows
	}
	// Clamp per-flow hits so no single flow drowns out the others.
	perFlow := query.MaxResults
	if perFlow <= 0 || perFlow > 10 {
		perFlow = 10
	}
	// Bounded fan-out, never entering the doc cache; slots keep candidate order.
	type slot struct{ res []models.SearchResult }
	results := make([]slot, len(flows))
	sem := make(chan struct{}, searchFanOut)
	var wg sync.WaitGroup
	for i, f := range flows {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, f *storageif.FlowDocument) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			doc, cached, err := s.resolveSearchCandidate(ctx, f.ID)
			if err != nil || doc == nil {
				return
			}
			q := query
			q.MaxResults = perFlow
			res := s.searchDoc(doc, q, cached)
			if res == nil {
				return
			}
			for j := range res.Results {
				res.Results[j].FlowID = f.ID
				res.Results[j].FlowName = f.Name
			}
			results[i].res = res.Results
		}(i, f)
	}
	wg.Wait()
	merged := make([]models.SearchResult, 0)
	for _, r := range results {
		merged = append(merged, r.res...)
	}
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
		// Reject empty/null-byte names before touching the filesystem (matches the other path guards).
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
				// Shallow-copy before mutating FilePath so the cached original stays pristine.
				docCopy := *doc
				docCopy.FilePath = path
				doc = &docCopy
				s.notifier.Emit("flow:loaded", FlowLoadedSummaryOf(doc))
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

	s.notifier.Emit("flow:loaded", FlowLoadedSummaryOf(doc))

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

// PatchFlow applies a textual patch to the raw source atomically, then re-parses; patch.File must be a bare filename.
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

	// Per-file lock: two concurrent patches reading the same contents would
	// silently lose the first write (lost update).
	plock := s.acquirePatchLock(targetPath)
	plock.mu.Lock()
	// Critical section covers read-modify-validate-write only (holding across the re-parse serialized the folder).
	unlock := func() {
		plock.mu.Unlock()
		s.releasePatchLock(targetPath, plock)
	}
	defer func() { unlock() }()

	data, readErr := os.ReadFile(targetPath) // #nosec G304 -- target derived from doc.FilePath + a validated bare filename
	if readErr != nil {
		return nil, fmt.Errorf("read source file: %w", readErr)
	}

	patched := analyzer.ApplyPatch(string(data), patch)

	// Reject only when the patch ADDS parse errors (fixes on imperfect files still land).
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

	// Capture the pre-fix state; record it only once the write landed.
	snap := s.snapshotDesktopFile(doc, targetPath, "before fix")

	dir := filepath.Dir(targetPath)
	if writeErr := atomicWriteConv(dir, targetPath, []byte(patched)); writeErr != nil {
		return nil, fmt.Errorf("write source file: %w", writeErr)
	}
	s.pushSnapshot(doc, snap)
	// Commit point: release the file lock NOW and finish outside the critical section.
	unlock()
	unlock = func() {} // neutralize the deferred double-release

	// Drop derived caches only after the write committed — a failed patch never invalidates.
	s.InvalidateSearchIndex(doc.ID)

	// Folder flows re-merge the whole folder; single files reuse the after-parse of the bytes written.
	if info.IsDir() {
		s.cacheFolderMember(fileName, patched, afterDoc)
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
	s.notifier.Emit("flow:loaded", FlowLoadedSummaryOf(afterDoc))
	s.docProvider.SetCurrentDoc(afterDoc)
	return afterDoc, nil
}

// SuppressFindingInSource writes a # pad-ignore[ruleID] directive before the block (ruleID "" = all rules).
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

func (s *FlowService) generateFixPatch(doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (models.Patch, error) {
	if doc == nil {
		return models.Patch{}, fmt.Errorf("no flow loaded")
	}
	block := s.FindBlockByID(doc, blockID)
	if block == nil {
		return models.Patch{}, fmt.Errorf("block %q not found", blockID)
	}

	patch, err := patchForFix(block, fixType, ruleID, variable, property)
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

// patchForFix: a declined fixer (empty patch) is a 400, not a silent success (regression).
func patchForFix(block *models.Block, fixType, ruleID, variable, property string) (models.Patch, error) {
	patch, err := analyzer.PatchForFix(block, fixType, ruleID, variable, property)
	if err != nil {
		return models.Patch{}, err
	}
	if len(patch.Ops) == 0 {
		return models.Patch{}, fmt.Errorf("%w: fixer declined: %q has nothing to change on this block (the finding may be missing the metadata this fix needs)", ErrInvalidInput, fixType)
	}
	return patch, nil
}

// ErrUnsafeFixRequiresOptIn is returned by ApplyFix for a behaviour-changing fix applied without the explicit opt-in; wraps ErrInvalidInput so HTTP answers 400.
var ErrUnsafeFixRequiresOptIn = fmt.Errorf("%w: this fix changes runtime behaviour; review the previewed diff and resend with unsafe: true", ErrInvalidInput)

// unsafeFixError names the fix type while keeping the sentinel matchable with errors.Is.
func unsafeFixError(fixType string) error {
	return fmt.Errorf("%w (fix type %q)", ErrUnsafeFixRequiresOptIn, fixType)
}

// ApplyFix applies one auto-fix; unsafe is the opt-in for behaviour-changing fixers.
func (s *FlowService) ApplyFix(ctx context.Context, doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string, unsafe bool) (*models.FlowDocument, error) {
	if analyzer.IsBehaviourChangingFix(fixType) && !unsafe {
		return nil, unsafeFixError(fixType)
	}
	// Cloud mode: build the aligned patching context (flow_cloudfix.go); desktop falls through to PatchFlow.
	if doc.FilePath == "" {
		cctx, err := s.cloudFixContextFor(doc, blockID, fixType, ruleID, variable, property)
		if err != nil {
			return nil, err
		}
		return s.applyCloudPatch(ctx, doc, cctx)
	}
	patch, err := s.generateFixPatch(doc, blockID, fixType, ruleID, variable, property)
	if err != nil {
		return nil, err
	}
	return s.PatchFlow(doc, patch)
}

// desktopSourcePath resolves the file the editor reads/writes; folder members are traversal-guarded bare filenames.
func desktopSourcePath(doc *models.FlowDocument, file string) (string, error) {
	info, err := os.Stat(doc.FilePath)
	if err != nil {
		return "", fmt.Errorf("source not accessible: %w", err)
	}
	if !info.IsDir() {
		return doc.FilePath, nil
	}
	if file == "" {
		file = "Main.txt"
	}
	if !safeConvComponent(file) || !strings.HasSuffix(strings.ToLower(file), ".txt") {
		return "", fmt.Errorf("%w: invalid flow file %q", ErrInvalidInput, file)
	}
	target := filepath.Join(doc.FilePath, file)
	finfo, err := os.Stat(target)
	if err != nil || finfo.IsDir() {
		return "", fmt.Errorf("%w: %q is not a file of this flow folder", ErrInvalidInput, file)
	}
	return target, nil
}

// GetSource returns the raw PAD source text: desktop reads the file (folder member `file`, Main.txt when empty); cloud returns doc.Source (serialized for imported flows).
func (s *FlowService) GetSource(doc *models.FlowDocument, file string) (string, error) {
	if doc.FilePath == "" {
		// Cloud: stored source; IMPORTED flows (Source never set) fall back to the serializer.
		if doc.Source != "" {
			return doc.Source, nil
		}
		return parser.SerializeDocument(doc), nil
	}
	targetPath, err := desktopSourcePath(doc, file)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(targetPath) // #nosec G304 -- validated member of doc.FilePath (desktopSourcePath)
	if err != nil {
		return "", fmt.Errorf("read source file: %w", err)
	}
	return string(data), nil
}

// SaveSource replaces the raw source text, re-parses and persists: desktop writes the file; cloud saves via SaveFlow (OCC).
func (s *FlowService) SaveSource(ctx context.Context, doc *models.FlowDocument, source, file string) (*models.FlowDocument, error) {
	if doc.FilePath == "" {
		if doc.IsFolder {
			return nil, fmt.Errorf("saving the combined source of a multi-file flow is not supported (it would collapse the folder structure)")
		}
		// Cloud: snapshot the stored source; record only once the save committed.
		snap := s.snapshotCloudSource(doc, "before source save")
		updated, err := s.persistCloudSource(ctx, doc, source)
		if err != nil {
			return nil, err
		}
		s.pushSnapshot(doc, snap)
		return updated, nil
	}
	targetPath, err := desktopSourcePath(doc, file)
	if err != nil {
		return nil, err
	}
	snap := s.snapshotDesktopFile(doc, targetPath, "before source save")
	if err := atomicWriteConv(filepath.Dir(targetPath), targetPath, []byte(source)); err != nil {
		return nil, fmt.Errorf("write source file: %w", err)
	}
	s.pushSnapshot(doc, snap)
	// The source-editor save replaced the file: derived caches are stale.
	s.InvalidateSearchIndex(doc.ID)
	if info, _ := os.Stat(doc.FilePath); info != nil && info.IsDir() {
		return s.LoadFlowFolder(doc.FilePath)
	}
	return s.LoadFlowFromPath(doc.FilePath)
}

// The fix loop is O(limit × document); maxFixBatchLimit bounds the caller-supplied limit.
const (
	defaultFixBatchLimit = 100
	maxFixBatchLimit     = 500
)

func clampFixBatchLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultFixBatchLimit
	case limit > maxFixBatchLimit:
		return maxFixBatchLimit
	default:
		return limit
	}
}

// FixBatchResult is what a batch fix did; Partial fixes persist (Doc is their re-parse) and the batch can re-run.
type FixBatchResult struct {
	Doc     *models.FlowDocument
	Applied int
	Partial bool
	Reason  error
}

// partialPersistTimeout bounds a partial batch's persist — ctx is already done; saving under it would lose fixes.
const partialPersistTimeout = 15 * time.Second

// persistContext gives a partial batch a context detached from ctx's cancellation, values kept.
func persistContext(ctx context.Context, partial bool) (context.Context, context.CancelFunc) {
	if !partial {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), partialPersistTimeout)
}

// ApplyFixBatch is ApplyFixBatchResult for callers needing only the document and count; a partial batch is a success (its fixes are persisted).
func (s *FlowService) ApplyFixBatch(ctx context.Context, doc *models.FlowDocument, ruleFilter map[string]bool, limit int) (*models.FlowDocument, int, error) {
	out, err := s.ApplyFixBatchResult(ctx, doc, ruleFilter, limit)
	return out.Doc, out.Applied, err
}

// ApplyFixBatchResult applies many auto-fixes in one pass, persisting once; a ctx cutoff persists what landed (Partial).
func (s *FlowService) ApplyFixBatchResult(ctx context.Context, doc *models.FlowDocument, ruleFilter map[string]bool, limit int) (FixBatchResult, error) {
	limit = clampFixBatchLimit(limit)

	var source, targetPath string
	cloud := doc.FilePath == ""
	if cloud {
		if doc.IsFolder {
			// Folder flows: the loop runs per member file (flow_cloudfix.go).
			return s.applyFixBatchCloudFolder(ctx, doc, ruleFilter, limit)
		}
		// Ingested flows: serialize the parsed content (the loop re-parses its own input).
		if doc.Source == "" {
			source = parser.SerializeDocument(doc)
		} else {
			source = doc.Source
		}
	} else {
		targetPath = doc.FilePath
		if info, err := os.Stat(doc.FilePath); err == nil && info.IsDir() {
			// Folder flows: the loop runs per member file, mirroring the cloud branch.
			return s.applyFixBatchDesktopFolder(ctx, doc, ruleFilter, limit)
		}
		data, err := os.ReadFile(targetPath) // #nosec G304 -- target derived from doc.FilePath
		if err != nil {
			return FixBatchResult{}, fmt.Errorf("read source file: %w", err)
		}
		source = string(data)
	}

	// One loop pass replaces four parses and reports both error counts.
	res, err := analyzer.ApplyFixesToSourceDocCtx(ctx, &source, doc.Name, s.fixOptions(ctx, doc, ruleFilter, limit))
	if err != nil && !res.Partial {
		return FixBatchResult{Applied: res.Fixed}, err
	}
	out := FixBatchResult{Applied: res.Fixed, Partial: res.Partial}
	if res.Partial {
		out.Reason = err
	}
	if res.Fixed == 0 {
		out.Doc = doc
		return out, nil // nothing changed
	}
	// Same gate as PatchFlow: a batch must not leave the source worse than before.
	if res.AfterErrors > res.BeforeErrors {
		return FixBatchResult{Applied: res.Fixed}, fmt.Errorf("batch fix would introduce parse errors (flow left unchanged)")
	}
	pctx, cancel := persistContext(ctx, out.Partial)
	defer cancel()

	// The pre-batch snapshot is recorded only once the write persisted.
	if cloud {
		// source is the exact text the loop patched (stored or canonical serialization).
		snap := s.snapshotCloudSourceText(doc, source, "before batch fix")
		// res.Doc is already the parse of the patched source — persist it directly.
		updated, err := s.persistCloudDoc(pctx, doc, res.Doc, source)
		if err != nil {
			return FixBatchResult{Applied: res.Fixed}, err
		}
		s.pushSnapshot(doc, snap)
		out.Doc = updated
		return out, nil
	}
	snap := s.snapshotDesktopFile(doc, targetPath, "before batch fix")
	if err := atomicWriteConv(filepath.Dir(targetPath), targetPath, []byte(source)); err != nil {
		return FixBatchResult{Applied: res.Fixed}, fmt.Errorf("write source file: %w", err)
	}
	s.pushSnapshot(doc, snap)
	// Seed the AST cache so the reload below is a hit, not another parse.
	if s.astCache != nil {
		hash := sha256.Sum256([]byte(source))
		s.astCache.Set(pctx, "ast:"+hex.EncodeToString(hash[:]), res.Doc, 24*time.Hour)
	}
	updated, err := s.LoadFlowFromPath(doc.FilePath)
	if err != nil {
		return FixBatchResult{Applied: res.Fixed}, err
	}
	out.Doc = updated
	return out, nil
}

// applyFixBatchDesktopFolder runs the fix loop per member (per-file gate, ONE undo snapshot); a mid-batch deadline keeps fixed members.
func (s *FlowService) applyFixBatchDesktopFolder(ctx context.Context, doc *models.FlowDocument, ruleFilter map[string]bool, limit int) (FixBatchResult, error) {
	entries, err := os.ReadDir(doc.FilePath)
	if err != nil {
		return FixBatchResult{}, fmt.Errorf("read folder: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".txt") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return FixBatchResult{Doc: doc}, nil
	}

	// Resolved once: every member of the folder shares one org profile.
	opts := s.fixOptions(ctx, doc, ruleFilter, limit)
	total := 0
	original := make(map[string]string) // pre-fix bytes of the files about to change
	fixed := make(map[string]string)
	fixedDocs := make(map[string]*models.FlowDocument) // the loop's parse of each fixed member
	var cutShort error                                 // ctx's error once it ended the batch early
	for _, name := range names {
		data, rerr := os.ReadFile(filepath.Join(doc.FilePath, name)) // #nosec G304 -- enumerated member of doc.FilePath
		if rerr != nil {
			return FixBatchResult{Applied: total}, fmt.Errorf("read %s: %w", name, rerr)
		}
		src := string(data)
		res, ferr := analyzer.ApplyFixesToSourceDocCtx(ctx, &src, name, opts)
		if ferr != nil {
			if !res.Partial {
				// Earlier fixes survive a deadline; a real failure fails the batch.
				if cerr := ctx.Err(); cerr == nil || !errors.Is(ferr, cerr) || total == 0 {
					return FixBatchResult{Applied: total}, ferr
				}
				cutShort = ferr
				break
			}
			cutShort = ferr // this member's fixes are usable; stop after it
		}
		// A member whose fixes add parse errors keeps its original text; the batch proceeds.
		if res.Fixed > 0 && res.AfterErrors <= res.BeforeErrors {
			original[name] = string(data)
			fixed[name] = src
			fixedDocs[name] = res.Doc
			total += res.Fixed
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

	// Record the undo entry once a member was rewritten: a later failure leaves earlier ones fixed on disk.
	snap := s.snapshotDesktopFiles(doc, original, "before batch fix")
	wrote := false
	for name, src := range fixed {
		target := filepath.Join(doc.FilePath, name)
		if werr := atomicWriteConv(doc.FilePath, target, []byte(src)); werr != nil {
			if wrote {
				s.pushSnapshot(doc, snap)
			}
			return FixBatchResult{Applied: total}, fmt.Errorf("write %s: %w", name, werr)
		}
		wrote = true
		// The loop's parse is of exactly the bytes written: seed the member cache.
		s.cacheFolderMember(name, src, fixedDocs[name])
	}
	s.pushSnapshot(doc, snap)
	s.InvalidateSearchIndex(doc.ID)
	updated, err := s.LoadFlowFolder(doc.FilePath)
	if err != nil {
		return FixBatchResult{Applied: total}, err
	}
	return FixBatchResult{Doc: updated, Applied: total, Partial: cutShort != nil, Reason: cutShort}, nil
}

// persistCloudDoc stamps a parsed doc with the original's cloud identity and saves via SaveFlow (OCC); updated must parse source.
func (s *FlowService) persistCloudDoc(ctx context.Context, doc *models.FlowDocument, updated *models.FlowDocument, source string) (*models.FlowDocument, error) {
	updated.ID = doc.ID
	updated.OwnerID = doc.OwnerID
	updated.OrganizationID = doc.OrganizationID
	updated.Source = source
	updated.RebuildIndexes()
	// Persist only when storage is configured; local mode never reaches here.
	if s.storage != nil {
		content, err := storedFlowContent(updated)
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
		// Saved content changed: drop derived caches so the next reader sees the post-edit flow.
		s.InvalidateSearchIndex(doc.ID)
		s.notifyCloudFlowMutation(doc.OwnerID)
	}
	return updated, nil
}

// storedFlowContent serializes doc for the content column; the trigram pushdown and snapshots match it.
func storedFlowContent(doc *models.FlowDocument) ([]byte, error) {
	return json.Marshal(doc)
}

func (s *FlowService) persistCloudSource(ctx context.Context, doc *models.FlowDocument, source string) (*models.FlowDocument, error) {
	updated, err := parser.ParseText(source, doc.Name, int64(len(source)))
	if err != nil {
		return nil, fmt.Errorf("re-parse patched source: %w", err)
	}
	return s.persistCloudDoc(ctx, doc, updated, source)
}

// PatchPreviewResult holds before/after source text for a dry-run fix preview; the frontend renders the diff for review.
type PatchPreviewResult struct {
	Original string `json:"original"`
	Patched  string `json:"patched"`
	// File names the member file a folder-flow preview refers to ("" for single-file).
	File string `json:"file,omitempty"`
}

// PreviewFix returns the before/after source text WITHOUT writing to disk.
func (s *FlowService) PreviewFix(doc *models.FlowDocument, blockID, fixType, ruleID, variable, property string) (*PatchPreviewResult, error) {
	// Cloud: build the aligned patching context (flow_cloudfix.go); folder flows preview the target member.
	if doc.FilePath == "" {
		cctx, err := s.cloudFixContextFor(doc, blockID, fixType, ruleID, variable, property)
		if err != nil {
			return nil, err
		}
		if cctx.isFolder() {
			fname := cctx.patch.File
			if fname == "" {
				fname = "Main.txt"
			}
			text, ok := cctx.files[fname]
			if !ok {
				return nil, fmt.Errorf("patch targets file %q which is not part of this flow", fname)
			}
			return &PatchPreviewResult{
				Original: text,
				Patched:  analyzer.ApplyPatch(text, cctx.patch),
				File:     fname,
			}, nil
		}
		patched := analyzer.ApplyPatch(cctx.source, cctx.patch)
		return &PatchPreviewResult{Original: cctx.source, Patched: patched}, nil
	}

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

// parseErrorCount counts error-severity problems — the lenient parser always returns a doc.
func parseErrorCount(source, fileName string) int {
	doc, err := parser.ParseText(source, fileName, int64(len(source)))
	if err != nil || doc == nil {
		// Treat a future hard ParseText failure as "maximally broken".
		return 1 << 30
	}
	return analyzer.CountParseErrors(doc)
}
