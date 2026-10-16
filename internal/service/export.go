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

// ExportService renders reports through the export registry (pad-core/export) and diffs flows.
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

// Render renders in as the registered format id, stamped with this build's version (SARIF
// tool.driver.version, so code-scanning consumers can tie results to a build). An unknown id is
// invalid input. It holds no state, so a nil *ExportService renders too.
func (s *ExportService) Render(id string, in export.Input, opts export.Options) ([]byte, export.Format, error) {
	f, ok := export.Lookup(id)
	if !ok {
		return nil, export.Format{}, fmt.Errorf("%w: unknown export format %q", ErrInvalidInput, id)
	}
	if opts.ToolVersion == "" {
		opts.ToolVersion = Version
	}
	content, err := f.Bytes(in, opts)
	if err != nil {
		return nil, f, fmt.Errorf("generate %s: %w", f.Label, err)
	}
	return content, f, nil
}

// Export renders a flow's report as the format id and, when path is set (desktop only: the
// handler drops it with accounts), also writes it there.
func (s *ExportService) Export(id string, doc *models.FlowDocument, report *models.AnalysisReport, path string) (content []byte, f export.Format, err error) {
	defer logger.Guard("App.Export", &err)

	if doc == nil {
		return nil, f, fmt.Errorf("no flow loaded")
	}

	if report == nil {
		return nil, f, fmt.Errorf("no analysis report available — run analysis first")
	}

	content, f, err = s.Render(id, export.Input{Report: report, Doc: doc}, export.Options{})
	if err != nil {
		return nil, f, err
	}

	if path != "" {
		if err = validateUserPath(path); err != nil {
			return nil, f, err
		}
		// Same shape as SaveClientExport: an absolute path (the native save
		// dialog returns absolute paths, so the frontend contract is unchanged)
		// and an extension matching a report format. The handler already drops
		// the path in cloud mode; this bounds what a local-mode drive-by POST
		// can do with a caller-supplied path — no dotfiles, no Startup-folder
		// .bat/.cmd, no overwriting arbitrary program files.
		if !filepath.IsAbs(path) {
			return nil, f, fmt.Errorf("%w: export path must be absolute", ErrInvalidInput)
		}
		if ext := strings.ToLower(filepath.Ext(path)); !docExportExts[ext] {
			return nil, f, fmt.Errorf("%w: unsupported export file type %q (want .md, .html or .pdf)", ErrInvalidInput, ext)
		}
		//nolint:gosec // G306: user-chosen export path; world-readable is intended for sharing.
		if err = os.WriteFile(path, content, 0644); err != nil {
			return nil, f, fmt.Errorf("write file: %w", err)
		}
	}

	return content, f, nil
}

// docExportExts are the file types the report exports (PDF/Markdown/HTML) may
// write server-side — the formats the native save dialog's filters offer.
var docExportExts = map[string]bool{".md": true, ".html": true, ".pdf": true}

// clientExportExts are the file types the findings panel saves through SaveClientExport:
// its CSV of the filtered list (rendered in the browser) and the SARIF and JUnit XML it
// fetched from /api/analysis/export. An allowlist, not "any path": this endpoint writes
// caller-supplied text, unlike the PDF/Markdown/HTML exports whose bytes the server renders.
var clientExportExts = map[string]bool{".csv": true, ".sarif": true, ".xml": true}

// SaveClientExport writes a findings export the browser holds to the path the desktop
// native save dialog returned. The webview cannot write files itself, so CSV/SARIF/JUnit
// used to fall back to an anchor download on desktop and skip the save dialog the
// PDF/Markdown/HTML exports show. Desktop only — the handler refuses it in cloud mode.
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
