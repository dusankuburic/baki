package export

import (
	"fmt"
	"strings"
	"time"

	"pad-core/models"
)

func ReportToMarkdown(report *models.AnalysisReport, doc *models.FlowDocument) string {
	var sb strings.Builder

	sb.WriteString("# PAD Analyzer — Analysis Report\n\n")

	sb.WriteString(fmt.Sprintf("**Flow:** %s\n", doc.Name))
	if doc.FilePath != "" {
		sb.WriteString(fmt.Sprintf("**File:** %s\n", doc.FilePath))
	}
	sb.WriteString(fmt.Sprintf("**Generated:** %s\n", report.GeneratedAt.Format(time.RFC1123)))
	sb.WriteString(fmt.Sprintf("**Duration:** %dms\n\n", report.DurationMs))

	sb.WriteString("## Summary\n\n")
	sb.WriteString("| Metric | Count |\n|---|---|\n")
	sb.WriteString(fmt.Sprintf("| Blocks analyzed | %d |\n", report.Stats.BlocksAnalyzed))
	sb.WriteString(fmt.Sprintf("| Rules run | %d |\n", report.Stats.RulesRun))
	sb.WriteString(fmt.Sprintf("| Errors | %d |\n", report.Stats.Errors))
	sb.WriteString(fmt.Sprintf("| Warnings | %d |\n", report.Stats.Warnings))
	sb.WriteString(fmt.Sprintf("| Info | %d |\n", report.Stats.Info))
	sb.WriteString("\n")

	if len(report.Findings) == 0 {
		sb.WriteString("No findings detected. The flow looks good!\n")
		return sb.String()
	}

	severityOrder := []models.Severity{models.SeverityError, models.SeverityWarning, models.SeverityInfo}
	for _, sev := range severityOrder {
		findings := filterBySeverity(report.Findings, sev)
		if len(findings) == 0 {
			continue
		}

		sb.WriteString(fmt.Sprintf("## %s (%d)\n\n", severityTitle(sev), len(findings)))

		for i, f := range findings {
			sb.WriteString(fmt.Sprintf("### %d. %s\n\n", i+1, f.Title))
			sb.WriteString(fmt.Sprintf("- **Rule:** `%s`\n", f.RuleID))
			sb.WriteString(fmt.Sprintf("- **Severity:** %s\n", f.Severity))
			sb.WriteString(fmt.Sprintf("- **Block:** %s\n", f.BlockID))
			if f.SubflowID != "" {
				sb.WriteString(fmt.Sprintf("- **Subflow:** %s\n", f.SubflowID))
			}
			sb.WriteString(fmt.Sprintf("\n%s\n", f.Description))
			if f.Suggestion != "" {
				sb.WriteString(fmt.Sprintf("\n> **Suggestion:** %s\n", f.Suggestion))
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func filterBySeverity(findings []models.Finding, sev models.Severity) []models.Finding {
	var result []models.Finding
	for _, f := range findings {
		if f.Severity == sev {
			result = append(result, f)
		}
	}
	return result
}

func severityTitle(sev models.Severity) string {
	switch sev {
	case models.SeverityError:
		return "Errors"
	case models.SeverityWarning:
		return "Warnings"
	case models.SeverityInfo:
		return "Info"
	default:
		return string(sev)
	}
}
