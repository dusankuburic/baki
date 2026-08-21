package export

import (
	"fmt"
	"html"
	"strings"
	"time"

	"pad-core/models"
)

func esc(s string) string { return html.EscapeString(s) }

// healthClass maps a 0–100 health score onto the badge tiers used by the
// report header (mirrors the thresholds the old analyzer-side renderer used).
func healthClass(score int) string {
	switch {
	case score < 50:
		return "bad"
	case score < 75:
		return "warn"
	default:
		return "good"
	}
}

// ReportToHTML renders a self-contained HTML report (inline CSS, no external
// assets) suitable for `bakicli -format html > report.html`, CI artifacts, and
// browser viewing. All finding-controlled text is HTML-escaped.
func ReportToHTML(report *models.AnalysisReport, doc *models.FlowDocument) string {
	if report == nil {
		report = &models.AnalysisReport{}
	}

	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>PAD Analyzer — Analysis Report</title>
<style>
:root { color-scheme: light dark; }
* { box-sizing: border-box; }
body { font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; margin: 0 auto; max-width: 960px; padding: 2rem 1rem; line-height: 1.5; color: #1f2937; background: #f9fafb; }
h1 { font-size: 1.5rem; margin: 0 0 .25rem; }
h2 { font-size: 1.15rem; margin: 2rem 0 .75rem; }
.meta { color: #6b7280; font-size: .875rem; margin-bottom: 1.5rem; }
.meta div { margin-top: .125rem; }
.cards { display: flex; flex-wrap: wrap; gap: .75rem; margin-bottom: 1.5rem; }
.card { flex: 1 1 8rem; background: #fff; border: 1px solid #e5e7eb; border-radius: .5rem; padding: .75rem 1rem; }
.card .num { font-size: 1.5rem; font-weight: 700; }
.card .lbl { font-size: .75rem; text-transform: uppercase; letter-spacing: .05em; color: #6b7280; }
.card.err .num { color: #dc2626; }
.card.warn .num { color: #d97706; }
.card.info .num { color: #2563eb; }
.finding { background: #fff; border: 1px solid #e5e7eb; border-left-width: .25rem; border-left-style: solid; border-radius: .375rem; padding: .75rem 1rem; margin-bottom: .75rem; }
.finding.err { border-left-color: #dc2626; }
.finding.warn { border-left-color: #d97706; }
.finding.info { border-left-color: #2563eb; }
.finding h3 { margin: 0 0 .25rem; font-size: 1rem; }
.badge { display: inline-block; font-size: .7rem; font-weight: 700; text-transform: uppercase; letter-spacing: .05em; border-radius: .25rem; padding: .1rem .4rem; color: #fff; }
.badge.err { background: #dc2626; }
.badge.warn { background: #d97706; }
.badge.info { background: #2563eb; }
.health { display: inline-block; border-radius: .25rem; padding: .1rem .5rem; font-size: .8rem; font-weight: 700; margin-bottom: 1rem; }
.health.good { background: #d1fae5; color: #065f46; }
.health.warn { background: #fef3c7; color: #92400e; }
.health.bad { background: #fee2e2; color: #991b1b; }
.props { font-size: .8rem; color: #6b7280; margin: .25rem 0 .5rem; }
.props code { background: #f3f4f6; border-radius: .2rem; padding: 0 .25rem; }
.suggestion { background: #ecfdf5; border: 1px solid #a7f3d0; border-radius: .375rem; padding: .5rem .75rem; margin-top: .5rem; font-size: .9rem; }
.ok { background: #ecfdf5; border: 1px solid #a7f3d0; border-radius: .5rem; padding: 1rem; color: #065f46; }
@media (prefers-color-scheme: dark) {
	body { background: #111827; color: #e5e7eb; }
	.card, .finding { background: #1f2937; border-color: #374151; }
	.props, .meta, .card .lbl { color: #9ca3af; }
	.props code { background: #374151; }
	.health.good { background: #065f46; color: #6ee7b7; }
	.health.warn { background: #713f12; color: #fbbf24; }
	.health.bad { background: #7f1d1d; color: #fca5a5; }
}
</style>
</head>
<body>
`)

	sb.WriteString("<h1>PAD Analyzer — Analysis Report</h1>\n<div class=\"meta\">\n")
	if doc != nil {
		fmt.Fprintf(&sb, "<div>Flow: %s</div>\n", esc(doc.Name))
		if doc.FilePath != "" {
			fmt.Fprintf(&sb, "<div>File: %s</div>\n", esc(doc.FilePath))
		}
	} else if report.FlowName != "" {
		fmt.Fprintf(&sb, "<div>Flow: %s</div>\n", esc(report.FlowName))
	}
	if !report.GeneratedAt.IsZero() {
		fmt.Fprintf(&sb, "<div>Generated: %s</div>\n", esc(report.GeneratedAt.Format(time.RFC1123)))
	}
	if report.DurationMs > 0 {
		fmt.Fprintf(&sb, "<div>Duration: %dms</div>\n", report.DurationMs)
	}
	sb.WriteString("</div>\n")

	if report.Metrics != nil {
		fmt.Fprintf(&sb, "<div class=\"health %s\">Health Score: %d/100</div>\n", healthClass(report.Metrics.HealthScore), report.Metrics.HealthScore)
	}

	sb.WriteString("<div class=\"cards\">\n")
	writeCard(&sb, "err", report.Stats.Errors, "Errors")
	writeCard(&sb, "warn", report.Stats.Warnings, "Warnings")
	writeCard(&sb, "info", report.Stats.Info, "Info")
	writeCard(&sb, "", report.Stats.BlocksAnalyzed, "Blocks analyzed")
	writeCard(&sb, "", report.Stats.RulesRun, "Rules run")
	sb.WriteString("</div>\n")

	if len(report.Findings) == 0 {
		sb.WriteString("<p class=\"ok\">No findings detected. The flow looks good!</p>\n")
		sb.WriteString("</body>\n</html>\n")
		return sb.String()
	}

	severityOrder := []models.Severity{models.SeverityError, models.SeverityWarning, models.SeverityInfo}
	grouped := groupFindingsBySeverity(report.Findings)
	for _, sev := range severityOrder {
		idxs := grouped[sev]
		if len(idxs) == 0 {
			continue
		}
		badgeClass := severityClass(sev)
		fmt.Fprintf(&sb, "<h2>%s <span class=\"badge %s\">%d</span></h2>\n", esc(severityTitle(sev)), badgeClass, len(idxs))
		for _, fi := range idxs {
			f := &report.Findings[fi]
			fmt.Fprintf(&sb, "<div class=\"finding %s\">\n<h3>%s</h3>\n", badgeClass, esc(f.Title))
			fmt.Fprintf(&sb, "<div><span class=\"badge %s\">%s</span></div>\n", badgeClass, esc(string(f.Severity)))
			sb.WriteString("<div class=\"props\">")
			fmt.Fprintf(&sb, "<span>Rule: <code>%s</code></span>", esc(f.RuleID))
			if f.Category != "" {
				fmt.Fprintf(&sb, " &middot; <span>Category: %s</span>", esc(f.Category))
			}
			fmt.Fprintf(&sb, " &middot; <span>Block: <code>%s</code></span>", esc(f.BlockID))
			if f.SubflowID != "" {
				fmt.Fprintf(&sb, " &middot; <span>Subflow: <code>%s</code></span>", esc(f.SubflowID))
			}
			sb.WriteString("</div>\n")
			if f.Description != "" {
				fmt.Fprintf(&sb, "<p>%s</p>\n", esc(f.Description))
			}
			if f.Suggestion != "" {
				fmt.Fprintf(&sb, "<div class=\"suggestion\"><strong>Suggestion:</strong> %s</div>\n", esc(f.Suggestion))
			}
			sb.WriteString("</div>\n")
		}
	}

	sb.WriteString("</body>\n</html>\n")
	return sb.String()
}

func writeCard(sb *strings.Builder, class string, value int, label string) {
	if class != "" {
		fmt.Fprintf(sb, "<div class=\"card %s\"><div class=\"num\">%d</div><div class=\"lbl\">%s</div></div>\n", class, value, esc(label))
	} else {
		fmt.Fprintf(sb, "<div class=\"card\"><div class=\"num\">%d</div><div class=\"lbl\">%s</div></div>\n", value, esc(label))
	}
}

func severityClass(sev models.Severity) string {
	switch sev {
	case models.SeverityError:
		return "err"
	case models.SeverityWarning:
		return "warn"
	case models.SeverityInfo:
		return "info"
	default:
		return "info"
	}
}
