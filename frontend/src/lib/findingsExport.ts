import {analysisApi, exportApi} from '@/api'
import type {Finding} from '@/types'
import {csvCell, downloadBlob} from '@/lib/csv'
import {getPlatformCapabilities} from '@/platform/guards'
import {logger} from '@/lib/logger'

async function deliverExport(content: string, docId: string, kind: {mime: string; ext: string; label: string}) {
  if (getPlatformCapabilities().nativeDialogs) {
    await exportApi.saveTextExport(content, {label: kind.label, ext: kind.ext})
    return
  }
  downloadBlob(content, kind.mime, `analysis-${docId}-${new Date().toISOString().slice(0, 10)}.${kind.ext}`)
}

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

export function exportFindingsCSV(findings: Finding[], docId: string) {
  saveFindingsCSV(findings, docId).catch(err => logger.warn('CSV export failed:', err))
}

export async function exportFindingsHTML(docId: string) {
  await exportApi.exportHTML(docId)
}

export async function exportFindingsJUnit(docId: string) {
  const xml = await analysisApi.exportJUnit()
  await deliverExport(xml, docId, {mime: 'application/xml;charset=utf-8;', ext: 'xml', label: 'JUnit XML'})
}

export async function exportFindingsSARIF(docId: string) {
  const sarif = await analysisApi.exportSARIF()
  await deliverExport(sarif, docId, {mime: 'application/sarif+json;charset=utf-8;', ext: 'sarif', label: 'SARIF'})
}
