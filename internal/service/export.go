package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pad-core/export"
	"pad-core/logger"
	"pad-core/models"
	"pad-core/parser"
)

func init() {
	// Surface the build version in SARIF tool.driver.version so consumers
	// (GitHub Code Scanning, Azure DevOps) can correlate results to a build.
	export.SetToolVersion(Version)
}

// ExportService handles flow diff, markdown, and PDF export operations.
type ExportService struct {
	notifier EventNotifier
	flow     *FlowService
	analysis *AnalysisService
}

func NewExportService(notifier EventNotifier, flow *FlowService, analysis *AnalysisService) *ExportService {
	return &ExportService{notifier: notifier, flow: flow, analysis: analysis}
}

func (s *ExportService) CompareCurrentWith(newDoc *models.FlowDocument, oldPath string) (diff *models.FlowDiff, err error) {
	defer logger.Guard("App.CompareCurrentWith", &err)

	if err := validateUserPath(oldPath); err != nil {
		return nil, err
	}

	if newDoc == nil {
		return nil, fmt.Errorf("no current flow loaded")
	}

	// Load and parse old version
	data, err := os.ReadFile(oldPath) // #nosec G304 -- reading a flow file the app was asked to diff
	if err != nil {
		return nil, fmt.Errorf("read old file: %w", err)
	}

	oldDoc, err := parser.ParseText(string(data), filepath.Base(oldPath), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("parse old version: %w", err)
	}

	return parser.DiffFlows(oldDoc, newDoc), nil
}

func (s *ExportService) ExportMarkdown(doc *models.FlowDocument, report *models.AnalysisReport, path string) (content []byte, err error) {
	defer logger.Guard("App.ExportMarkdown", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	if report == nil {
		return nil, fmt.Errorf("no analysis report available — run analysis first")
	}

	md := export.ReportToMarkdown(report, doc)
	content = []byte(md)

	if path != "" {
		if err = validateUserPath(path); err != nil {
			return nil, err
		}
		//nolint:gosec // G306: user-chosen export path; world-readable is intended for sharing.
		if err = os.WriteFile(path, content, 0644); err != nil {
			return nil, fmt.Errorf("write file: %w", err)
		}
	}

	return content, nil
}

func (s *ExportService) ExportHTML(doc *models.FlowDocument, report *models.AnalysisReport, path string) (content []byte, err error) {
	defer logger.Guard("App.ExportHTML", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	if report == nil {
		return nil, fmt.Errorf("no analysis report available — run analysis first")
	}

	content = []byte(export.ReportToHTML(report, doc))

	if path != "" {
		if err = validateUserPath(path); err != nil {
			return nil, err
		}
		//nolint:gosec // G306: user-chosen export path; world-readable is intended for sharing.
		if err = os.WriteFile(path, content, 0644); err != nil {
			return nil, fmt.Errorf("write file: %w", err)
		}
	}

	return content, nil
}

func (s *ExportService) ExportPDF(doc *models.FlowDocument, report *models.AnalysisReport, path string) (content []byte, err error) {
	defer logger.Guard("App.ExportPDF", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	if report == nil {
		return nil, fmt.Errorf("no analysis report available — run analysis first")
	}

	pdfBytes, err := export.ReportToPDF(report, doc)
	if err != nil {
		return nil, fmt.Errorf("generate PDF: %w", err)
	}

	if path != "" {
		if err = validateUserPath(path); err != nil {
			return nil, err
		}
		//nolint:gosec // G306: user-chosen export path; world-readable is intended for sharing.
		if err = os.WriteFile(path, pdfBytes, 0644); err != nil {
			return nil, fmt.Errorf("write file: %w", err)
		}
	}

	return pdfBytes, nil
}

// clientExportExts are the file types the findings panel renders in the
// browser (CSV of the filtered list, SARIF, JUnit XML) and SaveClientExport may
// write. An allowlist, not "any path": this endpoint writes caller-supplied
// text, unlike the PDF/Markdown/HTML exports whose bytes the server renders.
var clientExportExts = map[string]bool{".csv": true, ".sarif": true, ".xml": true}

// SaveClientExport writes a browser-rendered findings export to the path the
// desktop native save dialog returned. The webview cannot write files itself,
// so CSV/SARIF/JUnit used to fall back to an anchor download on desktop and
// skip the save dialog the PDF/Markdown/HTML exports show. Desktop only — the
// handler refuses it in cloud mode.
func (s *ExportService) SaveClientExport(path, content string) error {
	if err := validateUserPath(path); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: export path must be absolute", ErrInvalidInput)
	}
	if ext := strings.ToLower(filepath.Ext(path)); !clientExportExts[ext] {
		return fmt.Errorf("%w: unsupported export file type %q (want .csv, .sarif or .xml)", ErrInvalidInput, ext)
	}
	//nolint:gosec // G306: user-chosen export path; world-readable is intended for sharing.
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}
