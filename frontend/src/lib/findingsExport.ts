import {analysisApi} from '@/api'
import type {AnalysisReport} from '@/types/domain'
import {csvCell, downloadBlob} from '@/lib/csv'

// exportFindingsCSV serializes the report's findings; shared by the
// FindingsTab toolbar button and the analysis.export.csv command.
export function exportFindingsCSV(report: AnalysisReport, docId: string) {
  const rows = [['ID', 'Severity', 'Category', 'Title', 'Description', 'Block ID', 'Subflow ID', 'Suggestion']]
  for (const f of report.findings) {
    rows.push([
      csvCell(f.id),
      csvCell(f.severity),
      csvCell(f.category ?? ''),
      csvCell(f.title),
      csvCell(f.description),
      csvCell(f.blockId),
      csvCell(f.subflowId),
      csvCell(f.suggestion ?? ''),
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
