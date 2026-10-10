import {analysisApi, exportApi} from '@/api'
import type {Finding} from '@/types'
import {csvCell, downloadBlob} from '@/lib/csv'
import {getPlatformCapabilities} from '@/platform/guards'
import {logger} from '@/lib/logger'

// deliverExport saves an export rendered in the browser. Desktop takes the
// native save dialog branch, like PDF/Markdown/HTML — CSV, SARIF and JUnit used
// to go straight to an anchor download there, skipping the dialog (and the
// choice of where the file lands). Web keeps the anchor download.
async function deliverExport(content: string, docId: string, kind: {mime: string; ext: string; label: string}) {
  if (getPlatformCapabilities().nativeDialogs) {
    await exportApi.saveTextExport(content, {label: kind.label, ext: kind.ext})
    return
  }
  downloadBlob(content, kind.mime, `analysis-${docId}-${new Date().toISOString().slice(0, 10)}.${kind.ext}`)
}

// exportFindingsCSV serializes the GIVEN findings (not necessarily the whole
// report). The FindingsTab toolbar passes its actively-filtered list so a
// triager who filters to "errors only" gets exactly that in the CSV; the
// global shortcut/command callers pass the full report.findings (no filter
// context). Either way the exported set matches what the user is looking at.
// Awaitable (the desktop save can fail); FindingsTab awaits it to toast.
export async function saveFindingsCSV(findings: Finding[], docId: string) {
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
  await deliverExport(csv, docId, {mime: 'text/csv;charset=utf-8;', ext: 'csv', label: 'CSV'})
}

// exportFindingsCSV is the fire-and-forget form for the shortcut and command
// palette callers, which have no toast; a failed desktop save is logged like
// their HTML export failures.
export function exportFindingsCSV(findings: Finding[], docId: string) {
  saveFindingsCSV(findings, docId).catch(err => logger.warn('CSV export failed:', err))
}

// exportFindingsHTML downloads the backend-rendered HTML report. In web/cloud
// mode exportApi.exportHTML already triggers the browser anchor download; on
// desktop it writes through the native save dialog — either way the report
// lands with a deterministic filename derived from the flow id.
export async function exportFindingsHTML(docId: string) {
  await exportApi.exportHTML(docId)
}

// exportFindingsJUnit downloads the backend-rendered JUnit XML report (CI
// dashboards, test-report viewers) as the server wrote it.
export async function exportFindingsJUnit(docId: string) {
  const xml = await analysisApi.exportJUnit()
  await deliverExport(xml, docId, {mime: 'application/xml;charset=utf-8;', ext: 'xml', label: 'JUnit XML'})
}

// exportFindingsSARIF downloads a SARIF 2.1.0 report for GitHub Code Scanning
// or any SARIF-consuming tool. The backend serializes findings with stable
// fingerprints, rule metadata, and physical/logical locations; the file is its
// bytes, not a re-serialization.
export async function exportFindingsSARIF(docId: string) {
  const sarif = await analysisApi.exportSARIF()
  await deliverExport(sarif, docId, {mime: 'application/sarif+json;charset=utf-8;', ext: 'sarif', label: 'SARIF'})
}
