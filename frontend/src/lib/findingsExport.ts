import {analysisApi} from '@/api'
import type {Finding} from '@/types'
import {csvCell, downloadBlob} from '@/lib/csv'

// exportFindingsCSV serializes the GIVEN findings (not necessarily the whole
// report). The FindingsTab toolbar passes its actively-filtered list so a
// triager who filters to "errors only" gets exactly that in the CSV; the
// global shortcut/command callers pass the full report.findings (no filter
// context). Either way the exported set matches what the user is looking at.
export function exportFindingsCSV(findings: Finding[], docId: string) {
  const rows = [['ID', 'Severity', 'Category', 'Title', 'Description', 'Block ID', 'Subflow ID', 'Suggestion']]
  for (const f of findings) {
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
  downloadBlob(
    html as unknown as string,
    'text/html;charset=utf-8;',
    `analysis-${docId}-${new Date().toISOString().slice(0, 10)}.html`,
  )
}

// exportFindingsSARIF downloads a SARIF 2.1.0 report for GitHub Code Scanning
// or any SARIF-consuming tool. The backend serializes findings with stable
// fingerprints, rule metadata, and physical/logical locations.
export async function exportFindingsSARIF(docId: string) {
  const sarif = await analysisApi.exportSARIF()
  const text = JSON.stringify(sarif, null, 2)
  downloadBlob(
    text,
    'application/sarif+json;charset=utf-8;',
    `analysis-${docId}-${new Date().toISOString().slice(0, 10)}.sarif`,
  )
}
