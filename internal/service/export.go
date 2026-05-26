package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"pad-analyzer/internal/export"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/parser"
)

// ExportService handles flow diff, markdown, and PDF export operations.
type ExportService struct {
	ctx      context.Context
	notifier Notifier
	flow     *FlowService
	analysis *AnalysisService
}

func NewExportService(ctx context.Context, notifier Notifier, flow *FlowService, analysis *AnalysisService) *ExportService {
	return &ExportService{ctx: ctx, notifier: notifier, flow: flow, analysis: analysis}
}

func (s *ExportService) CompareCurrentWith(oldPath string) (diff *models.FlowDiff, err error) {
	defer logger.Guard("App.CompareCurrentWith", &err)

	newDoc := s.flow.CurrentDoc()

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

func (s *ExportService) ExportMarkdown(path string) (err error) {
	defer logger.Guard("App.ExportMarkdown", &err)

	curDoc := s.flow.CurrentDoc()

	if curDoc == nil {
		return fmt.Errorf("no flow loaded")
	}

	report := s.analysis.LastReport()
	if report == nil {
		return fmt.Errorf("no analysis report available — run analysis first")
	}

	md := export.ReportToMarkdown(report, curDoc)

	if err = os.WriteFile(path, []byte(md), 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

func (s *ExportService) ExportPDF(path string) (err error) {
	defer logger.Guard("App.ExportPDF", &err)

	curDoc := s.flow.CurrentDoc()

	if curDoc == nil {
		return fmt.Errorf("no flow loaded")
	}

	report := s.analysis.LastReport()
	if report == nil {
		return fmt.Errorf("no analysis report available — run analysis first")
	}

	pdfBytes, err := export.ReportToPDF(report, curDoc)
	if err != nil {
		return fmt.Errorf("generate PDF: %w", err)
	}

	if err = os.WriteFile(path, pdfBytes, 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}
