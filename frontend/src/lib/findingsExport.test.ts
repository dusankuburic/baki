import {describe, it, expect, vi, afterEach} from 'vitest'
import type {Finding} from '@/types'

const exportHTML = vi.fn()
const exportSARIF = vi.fn()
vi.mock('@/api', () => ({
  analysisApi: {exportSARIF},
  exportApi: {exportHTML},
}))

const downloadBlob = vi.fn()
vi.mock('@/lib/csv', () => ({
  csvCell: (v: string) => v,
  downloadBlob,
}))

function finding(p: Partial<Finding> = {}): Finding {
  return {
    id: 'F1',
    ruleId: 'r',
    blockId: 'blk-1',
    subflowId: 'sf-1',
    title: 'Title',
    description: 'Desc',
    severity: 'error',
    category: 'Security',
    ...p,
  } as Finding
}

afterEach(() => {
  vi.clearAllMocks()
})

describe('exportFindingsCSV', () => {
  it('builds a header row plus one row per finding and downloads it as CSV', async () => {
    const {exportFindingsCSV} = await import('./findingsExport')
    exportFindingsCSV([finding({suggestion: 'fix it'})], 'doc1')

    expect(downloadBlob).toHaveBeenCalledTimes(1)
    const [csv, mime, filename] = downloadBlob.mock.calls[0]
    expect(mime).toBe('text/csv;charset=utf-8;')
    expect(filename).toMatch(/^analysis-doc1-\d{4}-\d{2}-\d{2}\.csv$/)
    const lines = csv.split('\n')
    expect(lines).toHaveLength(2)
    expect(lines[0]).toBe('ID,Severity,Category,Title,Description,Block ID,Subflow ID,Suggestion')
    expect(lines[1]).toBe('F1,error,Security,Title,Desc,blk-1,sf-1,fix it')
  })

  it('handles findings with no category/suggestion', async () => {
    const {exportFindingsCSV} = await import('./findingsExport')
    exportFindingsCSV([finding({category: undefined, suggestion: undefined})], 'doc1')
    const csv = downloadBlob.mock.calls[0][0]
    expect(csv.split('\n')[1]).toBe('F1,error,,Title,Desc,blk-1,sf-1,')
  })
})

describe('exportFindingsHTML', () => {
  it('delegates to the dialog-aware export (native save dialog or browser download)', async () => {
    exportHTML.mockResolvedValue('')
    const {exportFindingsHTML} = await import('./findingsExport')
    await exportFindingsHTML('doc1')

    // exportApi.exportHTML owns the save UX (native dialog on desktop, anchor
    // download on web); the helper just routes the doc id through.
    expect(exportHTML).toHaveBeenCalledWith('doc1')
  })
})

describe('exportFindingsSARIF', () => {
  it('fetches the SARIF report, pretty-prints it, and downloads it', async () => {
    exportSARIF.mockResolvedValue({version: '2.1.0', runs: []})
    const {exportFindingsSARIF} = await import('./findingsExport')
    await exportFindingsSARIF('doc1')

    expect(exportSARIF).toHaveBeenCalled()
    const [content, mime, filename] = downloadBlob.mock.calls[0]
    expect(JSON.parse(content)).toEqual({version: '2.1.0', runs: []})
    expect(mime).toBe('application/sarif+json;charset=utf-8;')
    expect(filename).toMatch(/^analysis-doc1-\d{4}-\d{2}-\d{2}\.sarif$/)
  })
})
