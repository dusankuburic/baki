package service

import (
	"fmt"
	"os"
	"path/filepath"

	"pad-core/export"
	"pad-core/logger"
	"pad-core/models"
	"pad-core/parser"
)

// ExportService handles flow diff, markdown, and PDF export operations.
type ExportService struct {
	notifier Notifier
	flow     *FlowService
	analysis *AnalysisService
}

func NewExportService(notifier Notifier, flow *FlowService, analysis *AnalysisService) *ExportService {
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
	data, err := os.ReadFile(oldPath)
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
		if err = os.WriteFile(path, pdfBytes, 0644); err != nil {
			return nil, fmt.Errorf("write file: %w", err)
		}
	}

	return pdfBytes, nil
}
