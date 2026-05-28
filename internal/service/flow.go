package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/parser"
	"pad-analyzer/internal/search"
	"pad-analyzer/internal/storage"
)

// FlowService owns document state, search index, and all file-related operations.
type FlowService struct {
	ctx         context.Context
	notifier    Notifier
	settings    *storage.SettingsStore
	searchIndex *search.SearchIndex
	currentDoc  *models.FlowDocument
	docMu       sync.RWMutex
}

func NewFlowService(ctx context.Context, notifier Notifier, settings *storage.SettingsStore) *FlowService {
	return &FlowService{ctx: ctx, notifier: notifier, settings: settings}
}

// CurrentDoc returns the current document under a read lock.
func (s *FlowService) CurrentDoc() *models.FlowDocument {
	s.docMu.RLock()
	defer s.docMu.RUnlock()
	return s.currentDoc
}

// FindBlockByID looks up a block by ID using the BlocksByID map.
func (s *FlowService) FindBlockByID(blockID string) *models.Block {
	s.docMu.RLock()
	defer s.docMu.RUnlock()
	if s.currentDoc == nil || blockID == "" {
		return nil
	}
	if b, ok := s.currentDoc.BlocksByID[blockID]; ok {
		return b
	}
	return nil
}

// FindSubflowForBlock looks up which subflow contains the given block ID.
func (s *FlowService) FindSubflowForBlock(blockID string) *models.Subflow {
	s.docMu.RLock()
	defer s.docMu.RUnlock()
	if s.currentDoc == nil || blockID == "" {
		return nil
	}
	if sf, ok := s.currentDoc.BlockSubflow[blockID]; ok {
		return sf
	}
	return nil
}

func (s *FlowService) LoadFlowFromPath(path string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFromPath", &err)
	return s.loadAndParse(path)
}

func (s *FlowService) LoadFlowFolder(folderPath string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFolder", &err)

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

	s.docMu.Lock()
	s.currentDoc = doc
	s.searchIndex = search.NewSearchIndex(doc.ID, doc)
	s.docMu.Unlock()

	if s.settings != nil {
		totalSize := doc.Metadata.FileSize
		storage.AddRecentFile(s.settings, folderPath, totalSize)
	}

	return doc, nil
}

func (s *FlowService) LoadFlowFiles(files map[string]string, rootName string) (doc *models.FlowDocument, err error) {
	defer logger.Guard("App.LoadFlowFiles", &err)

	doc, err = parser.ParseFiles(files, rootName)
	if err != nil {
		return nil, err
	}

	s.docMu.Lock()
	s.currentDoc = doc
	s.searchIndex = search.NewSearchIndex(doc.ID, doc)
	s.docMu.Unlock()

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

func (s *FlowService) SearchFlow(flowID string, query models.SearchQuery) (results *models.SearchResults, err error) {
	defer logger.Guard("App.SearchFlow", &err)

	s.docMu.RLock()
	idx := s.searchIndex
	curDoc := s.currentDoc
	s.docMu.RUnlock()

	if idx == nil || idx.FlowID() != flowID {
		if curDoc != nil && curDoc.ID == flowID {
			idx = search.NewSearchIndex(flowID, curDoc)
			s.docMu.Lock()
			s.searchIndex = idx
			s.docMu.Unlock()
		} else {
			return nil, fmt.Errorf("no flow loaded with id %s", flowID)
		}
	}

	return idx.Search(query), nil
}

func (s *FlowService) GetSourceFiles() (result []models.SourceFileInfo, err error) {
	defer logger.Guard("App.GetSourceFiles", &err)

	s.docMu.RLock()
	doc := s.currentDoc
	s.docMu.RUnlock()

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

func (s *FlowService) ReadSourceFiles(filenames []string) (result map[string]string, err error) {
	defer logger.Guard("App.ReadSourceFiles", &err)

	s.docMu.RLock()
	doc := s.currentDoc
	s.docMu.RUnlock()

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

	s.docMu.Lock()
	s.currentDoc = doc
	s.searchIndex = search.NewSearchIndex(doc.ID, doc)
	s.docMu.Unlock()

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

// searchBlock is a recursive package-level helper that searches blocks by ID.
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
