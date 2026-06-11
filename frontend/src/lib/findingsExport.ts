import {analysisApi} from '@/api'
import type {AnalysisReport} from '@/types/domain'

function downloadBlob(content: string, mime: string, filename: string) {
  const blob = new Blob([content], {type: mime})
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}

// exportFindingsCSV serializes the report's findings; shared by the
// FindingsTab toolbar button and the analysis.export.csv command.
export function exportFindingsCSV(report: AnalysisReport, docId: string) {
  const rows = [['ID', 'Severity', 'Category', 'Title', 'Description', 'Block ID', 'Subflow ID', 'Suggestion']]
  for (const f of report.findings) {
    rows.push([
      f.id,
      f.severity,
      f.category ?? '',
      `"${f.title.replace(/"/g, '""')}"`,
      `"${f.description.replace(/"/g, '""')}"`,
      f.blockId,
      f.subflowId,
      f.suggestion ? `"${f.suggestion.replace(/"/g, '""')}"` : '',
    ])
  }
  const csv = rows.map(r => r.join(',')).join('\n')
  downloadBlob(csv, 'text/csv;charset=utf-8;', `analysis-${docId}-${new Date().toISOString().slice(0, 10)}.csv`)
}

// exportFindingsHTML downloads the backend-rendered HTML report.
export async function exportFindingsHTML(docId: string) {
  const html = await analysisApi.exportHTML()
  downloadBlob(html as unknown as string, 'text/html;charset=utf-8;', `analysis-${docId}-${new Date().toISOString().slice(0, 10)}.html`)
}
