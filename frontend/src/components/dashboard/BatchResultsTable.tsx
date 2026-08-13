import {FolderSearch, Download, AlertTriangle} from 'lucide-react'
import clsx from 'clsx'
import {formatCount} from '@/lib/format'
import {scoreColor} from '@/lib/scoring'
import type {BatchAnalysis} from '@/types'

// BatchResultsTable renders a completed folder batch: a summary header (totals +
// CSV export) and one row per flow, worst-finding-first. Extracted from
// AnalyticsDashboard; pure/presentational.
export function BatchResultsTable({
  batch,
  sortedResults,
  onExport,
}: {
  batch: BatchAnalysis
  sortedResults: BatchAnalysis['results']
  onExport: () => void
}) {
  return (
    <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-sm font-bold uppercase tracking-widest text-text-tertiary flex items-center gap-1.5">
          <FolderSearch size={14} />
          Batch Results
        </h3>
        <div className="flex items-center gap-3">
          <span className="text-sm text-text-tertiary tabular-nums">
            {formatCount(batch.totalFlows)} flows ·{' '}
            <span className="text-red-400">{formatCount(batch.totalErrors)}E</span>{' '}
            <span className="text-amber-400">{formatCount(batch.totalWarnings)}W</span>{' '}
            <span className="text-blue-400">{formatCount(batch.totalInfo)}I</span> · avg health{' '}
            <span className={scoreColor(batch.avgHealthScore)}>{batch.avgHealthScore.toFixed(0)}</span>
          </span>
          <button
            onClick={onExport}
            title="Export batch results as CSV"
            aria-label="Export batch results as CSV"
            className="text-text-tertiary hover:text-text-secondary p-1 rounded hover:bg-surface-3 transition-colors"
          >
            <Download size={12} />
          </button>
        </div>
      </div>
      <div className="space-y-1">
        {sortedResults.map((r, i) => (
          <div
            key={`${r.flowName}-${i}`}
            className={clsx(
              'flex items-center gap-2 px-2 py-1.5 rounded border',
              r.error ? 'border-red-500/20 bg-red-500/5' : 'border-border-subtle bg-surface-1',
            )}
          >
            {r.error && <AlertTriangle size={11} className="text-red-400 shrink-0" />}
            <span className="text-sm text-text-primary flex-1 truncate">{r.flowName}</span>
            {r.error ? (
              <span className="text-sm text-red-400/90 truncate max-w-[50%]">{r.error}</span>
            ) : (
              <>
                <span className="text-sm tabular-nums">
                  <span className="text-red-400">{r.report?.stats.errors ?? 0}E</span>{' '}
                  <span className="text-amber-400">{r.report?.stats.warnings ?? 0}W</span>{' '}
                  <span className="text-blue-400">{r.report?.stats.info ?? 0}I</span>
                </span>
                <span
                  className={clsx(
                    'text-sm font-bold font-mono tabular-nums w-7 text-right',
                    scoreColor(r.report?.metrics?.healthScore ?? 0),
                  )}
                >
                  {r.report?.metrics?.healthScore ?? '—'}
                </span>
              </>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}
