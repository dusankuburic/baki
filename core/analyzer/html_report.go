package analyzer

import (
	"fmt"
	"html"
	"strings"
	"time"

	"pad-core/models"
)

// GenerateHTMLReport previously lived here; single-flow HTML rendering is now
// owned by export.ReportToHTML (core/export/html.go) so the CLI and the in-app
// export share one renderer. Only the batch report remains analyzer-side.

func GenerateBatchHTMLReport(batch *models.BatchAnalysis) string {
	var sb strings.Builder

	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Batch Analysis Report</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #0f172a; color: #e2e8f0; padding: 24px; }
  .container { max-width: 1100px; margin: 0 auto; }
  h1 { font-size: 1.5rem; margin-bottom: 8px; }
  .meta { color: #94a3b8; font-size: 0.85rem; margin-bottom: 20px; }
  .stats { display: flex; gap: 16px; margin-bottom: 24px; flex-wrap: wrap; }
  .stat { background: #1e293b; border-radius: 8px; padding: 16px 20px; flex: 1; min-width: 100px; }
  .stat-label { font-size: 0.75rem; color: #94a3b8; text-transform: uppercase; }
  .stat-value { font-size: 1.4rem; font-weight: 700; margin-top: 4px; }
  .flow-card { background: #1e293b; border-radius: 8px; padding: 16px 20px; margin-bottom: 12px; }
  .flow-card h3 { font-size: 1rem; margin-bottom: 4px; }
  .flow-card .flow-meta { color: #94a3b8; font-size: 0.8rem; }
  .badge { display: inline-block; padding: 2px 8px; border-radius: 4px; font-size: 0.7rem; font-weight: 600; margin-left: 8px; }
  .badge-error { background: #7f1d1d; color: #fca5a5; }
  .badge-warning { background: #713f12; color: #fde68a; }
  @media print { body { background: #fff; color: #000; } }
</style>
</head>
<body><div class="container">
`)

	sb.WriteString("<h1>Batch Analysis Report</h1>\n")
	fmt.Fprintf(&sb, "<div class=\"meta\">%d flows &middot; %dms duration &middot; Generated: %s</div>\n",
		batch.TotalFlows, batch.DurationMs, time.Now().Format("2006-01-02 15:04:05"))

	sb.WriteString(`<div class="stats">`)
	fmt.Fprintf(&sb, `<div class="stat"><div class="stat-label">Flows</div><div class="stat-value">%d</div></div>`, batch.TotalFlows)
	fmt.Fprintf(&sb, `<div class="stat"><div class="stat-label">Errors</div><div class="stat-value" style="color:#f87171">%d</div></div>`, batch.TotalErrors)
	fmt.Fprintf(&sb, `<div class="stat"><div class="stat-label">Warnings</div><div class="stat-value" style="color:#fbbf24">%d</div></div>`, batch.TotalWarnings)
	fmt.Fprintf(&sb, `<div class="stat"><div class="stat-label">Avg Health</div><div class="stat-value" style="color:#34d399">%.0f</div></div>`, batch.AvgHealthScore)
	sb.WriteString(`</div>`)

	for _, r := range batch.Results {
		badges := ""
		if r.Report != nil {
			if r.Report.Stats.Errors > 0 {
				badges += fmt.Sprintf(`<span class="badge badge-error">%d errors</span>`, r.Report.Stats.Errors)
			}
			if r.Report.Stats.Warnings > 0 {
				badges += fmt.Sprintf(`<span class="badge badge-warning">%d warnings</span>`, r.Report.Stats.Warnings)
			}
		}
		fmt.Fprintf(&sb, `<div class="flow-card"><h3>%s%s</h3>`, html.EscapeString(r.FlowName), badges)
		if r.Error != "" {
			fmt.Fprintf(&sb, `<div class="flow-meta" style="color:#f87171;">Error: %s</div>`, html.EscapeString(r.Error))
		} else if r.Report != nil {
			hs := 0
			if r.Report.Metrics != nil {
				hs = r.Report.Metrics.HealthScore
			}
			fmt.Fprintf(&sb, `<div class="flow-meta">Health: %d/100 &middot; %d findings</div></div>`, hs, len(r.Report.Findings))
		}
	}

	sb.WriteString(`</div></body></html>`)
	return sb.String()
}
