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

	if doc != nil {
		fmt.Fprintf(&sb, "**Flow:** %s\n", doc.Name)
		if doc.FilePath != "" {
			fmt.Fprintf(&sb, "**File:** %s\n", doc.FilePath)
		}
	}
	if report == nil {
		report = &models.AnalysisReport{}
	}
	if !report.GeneratedAt.IsZero() {
		fmt.Fprintf(&sb, "**Generated:** %s\n", report.GeneratedAt.Format(time.RFC1123))
	}
	if report.DurationMs > 0 {
		fmt.Fprintf(&sb, "**Duration:** %dms\n\n", report.DurationMs)
	} else {
		sb.WriteString("\n")
	}

	sb.WriteString("## Summary\n\n")
	sb.WriteString("| Metric | Count |\n|---|---|\n")
	fmt.Fprintf(&sb, "| Blocks analyzed | %d |\n", report.Stats.BlocksAnalyzed)
	fmt.Fprintf(&sb, "| Rules run | %d |\n", report.Stats.RulesRun)
	fmt.Fprintf(&sb, "| Errors | %d |\n", report.Stats.Errors)
	fmt.Fprintf(&sb, "| Warnings | %d |\n", report.Stats.Warnings)
	fmt.Fprintf(&sb, "| Info | %d |\n", report.Stats.Info)
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

		fmt.Fprintf(&sb, "## %s (%d)\n\n", severityTitle(sev), len(findings))

		for i, f := range findings {
			fmt.Fprintf(&sb, "### %d. %s\n\n", i+1, f.Title)
			fmt.Fprintf(&sb, "- **Rule:** `%s`\n", f.RuleID)
			fmt.Fprintf(&sb, "- **Severity:** %s\n", f.Severity)
			fmt.Fprintf(&sb, "- **Block:** %s\n", f.BlockID)
			if f.SubflowID != "" {
				fmt.Fprintf(&sb, "- **Subflow:** %s\n", f.SubflowID)
			}
			fmt.Fprintf(&sb, "\n%s\n", f.Description)
			if f.Suggestion != "" {
				fmt.Fprintf(&sb, "\n> **Suggestion:** %s\n", f.Suggestion)
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
