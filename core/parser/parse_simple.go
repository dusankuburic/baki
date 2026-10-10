package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pad-core/logger"
	"pad-core/models"

	"github.com/google/uuid"
)

type Parser struct {
	text       string
	fileName   string
	fileSize   int64
	onProgress ProgressCallback
}

// ParserOption configures a Parser. Options are applied left-to-right.
type ParserOption func(*Parser)

// WithProgress installs a ProgressCallback receiving percent-done updates
// during Parse; a nil callback keeps the hot path branch-free.
func WithProgress(cb ProgressCallback) ParserOption {
	return func(p *Parser) { p.onProgress = cb }
}

func NewParser(text, fileName string, fileSize int64, opts ...ParserOption) *Parser {
	p := &Parser{
		text:     text,
		fileName: fileName,
		fileSize: fileSize,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
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

	cb := p.onProgress
	if cb != nil {
		cb(10, "Tokenized")
	}

	state := newParseState()
	if cb == nil {
		for _, tok := range tokens {
			state.processToken(tok)
		}
	} else {
		totalTokens := 0
		for _, tok := range tokens {
			if tok.Kind != TokEmpty {
				totalTokens++
			}
		}
		processed := 0
		lastReported := 10
		for _, tok := range tokens {
			if tok.Kind == TokEmpty {
				continue
			}
			processed++
			pct := 10 + (processed * 85 / max(totalTokens, 1))
			if pct >= lastReported+5 {
				cb(pct, "Parsing...")
				lastReported = pct
			}
			state.processToken(tok)
		}
	}
	// Flush closable blocks still open at EOF: without this, a file ending
	// abruptly (open LOOP/IF/BLOCK, no END/#EndRegion) silently drops them
	// from ParseErrors. Runs in both paths — the legacy copy missed it.
	if state.current != nil {
		state.recordUnclosedBlocks()
	}

	if cb != nil {
		cb(95, "Finalizing...")
	}
	subflows, totalBlocks, maxDepth := finalizeSubflows(state.built)
	doc := buildDocument(p.text, p.fileName, p.fileSize, subflows, state.parseErrors, totalBlocks, maxDepth)
	if cb != nil {
		cb(100, "Done")
	}
	return doc, nil
}

// MaxParseTextSize is the maximum source-text size the parser will accept.
// Prevents pathological inputs from consuming unbounded memory/time. Callers
// that need to parse larger inputs (e.g. very large multi-file folders) should
// use ParseFiles which parses each file independently under this limit.
const MaxParseTextSize = 50 * 1024 * 1024 // 50 MB

func ParseText(text, fileName string, fileSize int64) (*models.FlowDocument, error) {
	if len(text) > MaxParseTextSize {
		return nil, fmt.Errorf("source too large: %d bytes (limit %d bytes); use ParseFiles for multi-file flows", len(text), MaxParseTextSize)
	}
	p := NewParser(text, fileName, fileSize)
	return p.Parse()
}

// MemberParser parses one member file of a folder flow with ParseText's
// signature. ParseFolderWith takes a caller's parser so the desktop
// service can memoise per-member parses on folder reload.
type MemberParser func(text, name string, size int64) (*models.FlowDocument, error)

// ParseFolder reads and parses every .txt member of folderPath (as
// ExportFolder lists them) into one combined document (Main first, then the
// rest by name), keeping the desktop's shape: absolute Files[].Path, base-name
// SourceFile, IsFolder always. Equivalent to ParseFolderWith(folderPath,
// ParseText).
func ParseFolder(folderPath string) (*models.FlowDocument, error) {
	return ParseFolderWith(folderPath, ParseText)
}

// ParseFolderWith is ParseFolder with parseMember in place of ParseText.
// A member doc is only READ here (subflows copied by value), so a
// memoising parser may hand out the same doc on every call.
func ParseFolderWith(folderPath string, parseMember MemberParser) (*models.FlowDocument, error) {
	logger.Debug("ParseFolder started", "path", folderPath)
	if parseMember == nil {
		parseMember = ParseText
	}
	var txtFiles []fs.DirEntry
	err := walkFolder(folderPath, ExportFolder(), func(_ string, d fs.DirEntry) error {
		txtFiles = append(txtFiles, d)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read folder: %w", err)
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
		data, err := os.ReadFile(filePath) // #nosec G304 -- reading .txt files from the folder being parsed
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

		doc, err := parseMember(text, entry.Name(), size)
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

// trimFlowExt strips a PAD source extension (.txt or .pad) so member files
// sort by their logical name and "Main" (either extension) sorts first.
func trimFlowExt(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".pad") {
		return name[:len(name)-len(".pad")]
	}
	return strings.TrimSuffix(name, ".txt")
}

func ParseFiles(files map[string]string, rootName string) (*models.FlowDocument, error) {
	var filenames []string
	for name := range files {
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".txt") || strings.HasSuffix(lower, ".pad") {
			filenames = append(filenames, name)
		}
	}
	if len(filenames) == 0 {
		return nil, fmt.Errorf("no .txt or .pad files provided")
	}

	sort.Slice(filenames, func(i, j int) bool {
		ni := trimFlowExt(filenames[i])
		nj := trimFlowExt(filenames[j])
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

	// Session analytics need an identity that survives re-parsing; these
	// path-less docs key on the sorted file-name set. Name-based on
	// purpose: content must NOT participate or the identity rotates on
	// every edit.
	nameHash := sha256.New()
	for _, name := range filenames {
		nameHash.Write([]byte(name))
		nameHash.Write([]byte{0})
	}
	stableID := "files-" + hex.EncodeToString(nameHash.Sum(nil))[:16]

	doc := &models.FlowDocument{
		ID:          uuid.NewString(),
		StableID:    stableID,
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
