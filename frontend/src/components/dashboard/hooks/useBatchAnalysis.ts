import {useCallback, useMemo, useState} from 'react'
import {analysisApi} from '@/api'
import {createAdapter} from '@/platform/adapters'
import {useToast} from '@/components/shared'
import {csvCell, downloadBlob} from '@/lib/csv'
import type {BatchAnalysis} from '@/types'

export function exportBatchCSV(batch: BatchAnalysis) {
  const rows = [['Flow', 'Errors', 'Warnings', 'Info', 'Health', 'Load Error']]
  for (const r of batch.results) {
    rows.push([
      csvCell(r.flowName),
      String(r.report?.stats.errors ?? ''),
      String(r.report?.stats.warnings ?? ''),
      String(r.report?.stats.info ?? ''),
      String(r.report?.metrics?.healthScore ?? ''),
      r.error ? csvCell(r.error) : '',
    ])
  }
  const csv = rows.map(r => r.join(',')).join('\n')
  downloadBlob(csv, 'text/csv;charset=utf-8;', `batch-analysis-${new Date().toISOString().slice(0, 10)}.csv`)
}

export interface BatchAnalysisState {
  batch: BatchAnalysis | null
  batchRunning: boolean
  sortedResults: BatchAnalysis['results']
  runBatch: () => Promise<void>
  exportCSV: () => void
}

export function useBatchAnalysis(onBatchComplete: () => void): BatchAnalysisState {
  const toast = useToast()
  const [batch, setBatch] = useState<BatchAnalysis | null>(null)
  const [batchRunning, setBatchRunning] = useState(false)

  const runBatch = useCallback(async () => {
    const adapter = createAdapter()
    const result = await adapter.fileOpenDirectory()
    if (!result) return
    if (typeof result !== 'string' || result.trim().startsWith('{')) {
      toast.error('Batch analysis requires a local file system')
      return
    }
    setBatchRunning(true)
    try {
      const b = await analysisApi.batchAnalyze(result)
      setBatch(b)
      onBatchComplete()
    } catch (err) {
      toast.error('Batch analysis failed: ' + (err as Error).message)
    } finally {
      setBatchRunning(false)
    }
  }, [toast, onBatchComplete])

  const exportCSV = useCallback(() => {
    if (!batch) return
    exportBatchCSV(batch)
    toast.success('Batch results exported')
  }, [batch, toast])

  const sortedResults = useMemo(
    () =>
      batch
        ? [...batch.results].sort((a, b) => (b.report?.findings.length ?? -1) - (a.report?.findings.length ?? -1))
        : [],
    [batch],
  )

  return {batch, batchRunning, sortedResults, runBatch, exportCSV}
}
