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
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/cache"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/parser"
	"pad-analyzer/internal/search"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// FlowService owns document state, search index, and all file-related operations.
type FlowService struct {
	notifier    Notifier
	settings    *storage.SettingsStore
	docProvider DocumentProvider
	storage     storageif.StorageBackend
	orgSvc      *collaboration.OrgService
	astCache    cache.Cache

	// idxCache memoises built search indexes by flow ID. Building an index walks
	// and tokenises every block, so rebuilding it on every search-as-you-type
	// keystroke is wasteful; a flow's content is immutable for a given ID
	// (a new upload/parse yields a new ID), and in-place cloud updates call
	// InvalidateSearchIndex.
	idxMu    sync.RWMutex
	idxCache map[string]*search.SearchIndex
}

func NewFlowService(notifier Notifier, settings *storage.SettingsStore, docProvider DocumentProvider, storage storageif.StorageBackend, orgSvc *collaboration.OrgService, astCache cache.Cache) *FlowService {
	return &FlowService{
		notifier:    notifier,
		settings:    settings,
		docProvider: docProvider,
		storage:     storage,
		orgSvc:      orgSvc,
		astCache:    astCache,
		idxCache:    make(map[string]*search.SearchIndex),
	}
}

// GetAuthorized loads a flow and verifies the user has at least minPerm access.
// minPerm is "viewer", "editor", or "admin".
func (s *FlowService) GetAuthorized(ctx context.Context, flowID, userID, minPerm string) (*models.FlowDocument, error) {
	if s.storage == nil { // Local mode
		return s.docProvider.ResolveDoc(ctx, flowID)
	}

	// 1. Load the doc metadata/header from storage to check permissions
	doc, err := s.docProvider.ResolveDoc(ctx, flowID)
	if err != nil {
		return nil, err
	}

	// 2. Check ownership
	if doc.OwnerID == "" || doc.OwnerID == userID {
		return doc, nil
	}

	// 3. Check org membership
	if doc.OrganizationID != "" && s.orgSvc != nil {
		if role, err := s.orgSvc.MemberRole(doc.OrganizationID, userID); err == nil {
			if orgRoleToPermRank(role) >= permRank(minPerm) {
				return doc, nil
			}
		}
	}

	// 4. Check collaborators
	collabs, err := s.storage.ListCollaborators(ctx, flowID)
	if err == nil {
		need := permRank(minPerm)
		for _, c := range collabs {
			if c.UserID == userID && permRank(c.Permission) >= need {
				return doc, nil
			}
		}
	}

	return nil, ErrPermissionDenied
}

func permRank(p string) int {
	switch p {
	case "admin":
		return 30
	case "editor":
		return 20
	case "viewer":
		return 10
	default:
		return 0
	}
}

func orgRoleToPermRank(role auth.Role) int {
	switch role {
	case auth.RoleAdmin:
		return 30
	case auth.RoleMember:
		return 20
	default:
		return 10
	}
}

func (s *FlowService) DocProvider() DocumentProvider {
	return s.docProvider
}

func (s *FlowService) InvalidateSearchIndex(flowID string) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	delete(s.idxCache, flowID)
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
		storage.AddRecentFile(s.settings, folderPath, totalSize)
	}

	s.docProvider.SetCurrentDoc(doc)
	return doc, nil
}

func (s *FlowService) LoadFlowFiles(ctx context.Context, files map[string]string, rootName string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFiles", &err)

	// Generate a combined hash of all files to use as a cache key
	h := sha256.New()
	// Sort keys for deterministic hash
	fileNames := make([]string, 0, len(files))
	for k := range files {
		fileNames = append(fileNames, k)
	}
	for _, name := range fileNames {
		h.Write([]byte(name))
		h.Write([]byte(files[name]))
	}
	key := "ast-files:" + hex.EncodeToString(h.Sum(nil))

	if s.astCache != nil {
		if cached, ok := s.astCache.Get(ctx, key); ok {
			if doc, ok := cached.(*models.FlowDocument); ok {
				s.docProvider.SetCurrentDoc(doc)
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
	return storage.RemoveRecentFile(s.settings, path)
}

func (s *FlowService) ClearRecentFiles() (err error) {
	defer logger.Guard("App.ClearRecentFiles", &err)
	return storage.ClearRecentFiles(s.settings)
}

func (s *FlowService) RevealInFileManager(path string) (err error) {
	defer logger.Guard("App.RevealInFileManager", &err)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", "/select,", path)
	case "darwin":
		cmd = exec.Command("open", "-R", path)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(path))
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
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
	s.idxMu.RLock()
	if idx, ok := s.idxCache[doc.ID]; ok {
		s.idxMu.RUnlock()
		return idx
	}
	s.idxMu.RUnlock()

	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	// Check again under write lock
	if idx, ok := s.idxCache[doc.ID]; ok {
		return idx
	}
	idx := search.NewSearchIndex(doc.ID, doc)
	s.idxCache[doc.ID] = idx
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
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
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

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("couldn't read file: %w", err)
	}

	hash := sha256.Sum256(data)
	key := "ast:" + hex.EncodeToString(hash[:])

	if s.astCache != nil {
		if cached, ok := s.astCache.Get(context.Background(), key); ok {
			if doc, ok := cached.(*models.FlowDocument); ok {
				// We need a deep copy if we're going to modify FilePath
				// But for now, let's just use it and set FilePath
				doc.FilePath = path
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
		storage.AddRecentFile(s.settings, path, info.Size())
	}

	s.notifier.Emit("flow:loaded", doc)

	logger.Info("flow parsed",
		"file", fileName,
		"subflows", doc.Metadata.SubflowCount,
		"blocks", doc.Metadata.BlockCount,
	)

	return doc, nil
}

func (s *FlowService) readSubflowSource(doc *models.FlowDocument, sf *models.Subflow) string {
	if sf.SourceFile != "" && doc.FilePath != "" {
		candidate := filepath.Join(filepath.Dir(doc.FilePath), sf.SourceFile)
		data, err := os.ReadFile(candidate)
		if err == nil {
			return string(data)
		}
	}
	if doc.FilePath != "" {
		data, err := os.ReadFile(doc.FilePath)
		if err == nil {
			return string(data)
		}
	}
	return ""
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
