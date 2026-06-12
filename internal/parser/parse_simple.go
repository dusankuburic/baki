package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"

	"github.com/google/uuid"
)

type Parser struct {
	text     string
	fileName string
	fileSize int64
}

func NewParser(text, fileName string, fileSize int64) *Parser {
	return &Parser{
		text:     text,
		fileName: fileName,
		fileSize: fileSize,
	}
}

func (p *Parser) Parse() (*models.FlowDocument, error) {
	tokens := Tokenize(p.text)

	hasSubflow := false
	for _, tok := range tokens {
		if tok.Kind == TokSubflowStart {
			hasSubflow = true
			break
		}
	}

	if !hasSubflow {
		tokens = wrapImplicitSubflow(tokens, p.fileName)
	}

	state := newParseState()
	for _, tok := range tokens {
		state.processToken(tok)
	}

	subflows, totalBlocks, maxDepth := finalizeSubflows(state.built)
	return buildDocument(p.text, p.fileName, p.fileSize, subflows, state.parseErrors, totalBlocks, maxDepth), nil
}

func ParseText(text, fileName string, fileSize int64) (*models.FlowDocument, error) {
	p := NewParser(text, fileName, fileSize)
	return p.Parse()
}

func ParseFolder(folderPath string) (*models.FlowDocument, error) {
	logger.Debug("ParseFolder started", "path", folderPath)
	entries, err := os.ReadDir(folderPath)
	if err != nil {
		return nil, fmt.Errorf("read folder: %w", err)
	}

	var txtFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".txt") {
			txtFiles = append(txtFiles, e)
		}
	}
	logger.Debug("Found .txt files", "count", len(txtFiles))
	if len(txtFiles) == 0 {
		return nil, fmt.Errorf("no .txt files found in %s", folderPath)
	}

	sort.Slice(txtFiles, func(i, j int) bool {
		ni := strings.TrimSuffix(txtFiles[i].Name(), ".txt")
		nj := strings.TrimSuffix(txtFiles[j].Name(), ".txt")
		if ni == "Main" {
			return true
		}
		if nj == "Main" {
			return false
		}
		return ni < nj
	})

	folderName := filepath.Base(folderPath)
	var allSubflows []models.Subflow
	var allErrors []models.ParseError
	totalBlocks := 0
	maxDepth := 0
	totalLines := 0
	var totalSize int64

	var allFiles []models.FlowFileInfo

	for _, entry := range txtFiles {
		filePath := filepath.Join(folderPath, entry.Name())
		logger.Debug("Parsing file", "name", entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}

		text := string(data)
		info, _ := entry.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		totalSize += size

		allFiles = append(allFiles, models.FlowFileInfo{
			Path: filePath,
			Name: entry.Name(),
			Size: size,
		})

		doc, err := ParseText(text, entry.Name(), size)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}

		for _, sf := range doc.Subflows {
			sf.SourceFile = entry.Name()
			allSubflows = append(allSubflows, sf)
		}
		allErrors = append(allErrors, doc.ParseErrors...)
		totalBlocks += doc.Metadata.BlockCount
		if doc.Metadata.MaxDepth > maxDepth {
			maxDepth = doc.Metadata.MaxDepth
		}
		totalLines += doc.Metadata.RawLineCount
	}

	logger.Debug("Building final document")
	doc := &models.FlowDocument{
		ID:          uuid.NewString(),
		Name:        folderName,
		FilePath:    folderPath,
		Subflows:    allSubflows,
		ParseErrors: allErrors,
		Files:       allFiles,
		IsFolder:    true,
		Metadata: models.FlowMetadata{
			BlockCount:   totalBlocks,
			SubflowCount: len(allSubflows),
			MaxDepth:     maxDepth,
			ParsedAt:     time.Now(),
			FileSize:     totalSize,
			RawLineCount: totalLines,
		},
		BlocksByID:   make(map[string]*models.Block, totalBlocks),
		BlockSubflow: make(map[string]*models.Subflow, totalBlocks),
		SubflowsByID: make(map[string]*models.Subflow, len(allSubflows)),
	}

	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		doc.SubflowsByID[sf.ID] = sf
		for j := range sf.Blocks {
			indexBlockInDoc(doc, sf, &sf.Blocks[j])
		}
	}

	logger.Debug("ParseFolder finished")
	return doc, nil
}

func ParseFiles(files map[string]string, rootName string) (*models.FlowDocument, error) {
	var filenames []string
	for name := range files {
		if strings.HasSuffix(strings.ToLower(name), ".txt") {
			filenames = append(filenames, name)
		}
	}
	if len(filenames) == 0 {
		return nil, fmt.Errorf("no .txt files provided")
	}

	sort.Slice(filenames, func(i, j int) bool {
		ni := strings.TrimSuffix(filenames[i], ".txt")
		nj := strings.TrimSuffix(filenames[j], ".txt")
		if ni == "Main" {
			return true
		}
		if nj == "Main" {
			return false
		}
		return ni < nj
	})

	var allSubflows []models.Subflow
	var allErrors []models.ParseError
	totalBlocks := 0
	maxDepth := 0
	totalLines := 0
	var totalSize int64

	var allFiles []models.FlowFileInfo

	for _, name := range filenames {
		text := files[name]
		size := int64(len(text))
		totalSize += size

		allFiles = append(allFiles, models.FlowFileInfo{
			Name: name,
			Path: name,
			Size: size,
		})

		doc, err := ParseText(text, name, size)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}

		for _, sf := range doc.Subflows {
			sf.SourceFile = name
			allSubflows = append(allSubflows, sf)
		}
		allErrors = append(allErrors, doc.ParseErrors...)
		totalBlocks += doc.Metadata.BlockCount
		if doc.Metadata.MaxDepth > maxDepth {
			maxDepth = doc.Metadata.MaxDepth
		}
		totalLines += doc.Metadata.RawLineCount
	}

	doc := &models.FlowDocument{
		ID:          uuid.NewString(),
		Name:        rootName,
		Subflows:    allSubflows,
		ParseErrors: allErrors,
		Files:       allFiles,
		IsFolder:    len(filenames) > 1,
		Metadata: models.FlowMetadata{
			BlockCount:   totalBlocks,
			SubflowCount: len(allSubflows),
			MaxDepth:     maxDepth,
			ParsedAt:     time.Now(),
			FileSize:     totalSize,
			RawLineCount: totalLines,
		},
		BlocksByID:   make(map[string]*models.Block, totalBlocks),
		BlockSubflow: make(map[string]*models.Subflow, totalBlocks),
		SubflowsByID: make(map[string]*models.Subflow, len(allSubflows)),
	}

	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		doc.SubflowsByID[sf.ID] = sf
		for j := range sf.Blocks {
			indexBlockInDoc(doc, sf, &sf.Blocks[j])
		}
	}

	return doc, nil
}

func wrapImplicitSubflow(tokens []Token, fileName string) []Token {
	name := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	if name == "" {
		name = "Main"
	}
	start := Token{
		Kind:    TokSubflowStart,
		Line:    1,
		Indent:  0,
		Raw:     "",
		Content: "",
		Name:    name,
		RawType: "Region",
	}
	end := Token{
		Kind:    TokSubflowEnd,
		Line:    0,
		Indent:  0,
		Raw:     "",
		Content: "",
		Name:    "",
		RawType: "EndRegion",
	}
	result := make([]Token, 0, len(tokens)+2)
	result = append(result, start)
	result = append(result, tokens...)
	result = append(result, end)
	return result
}
