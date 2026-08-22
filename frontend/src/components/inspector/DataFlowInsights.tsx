import React from 'react'
import {ShieldAlert} from 'lucide-react'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'
import type {DataFlowAnalysis, TaintPath} from '@/types'

// TaintPathsPanel renders the full set of source→sink taint flows.
// Each row shows the source variable, the sink type, and a
// path-length badge; clicking jumps to the sink block for review.
function TaintPathsPanel({paths, onNavigate}: {paths: TaintPath[]; onNavigate: (id: string) => void}) {
  const [showAll, setShowAll] = React.useState(false)
  const INITIAL = 5
  const visible = showAll ? paths : paths.slice(0, INITIAL)
  const hidden = paths.length - visible.length

  return (
    <div className="p-3 rounded-lg border border-amber-500/20 bg-amber-500/5">
      <div className="flex items-center justify-between mb-1">
        <span className="text-xs font-bold text-amber-400">Taint Paths ({paths.length})</span>
        <span className="text-2xs text-text-tertiary">untrusted input → sensitive sink</span>
      </div>
      {visible.map((tp, i) => (
        <button
          key={i}
          onClick={() => onNavigate(tp.sinkBlock)}
          title={tp.path && tp.path.length > 0 ? `via ${tp.path.length} step(s)` : undefined}
          className="block w-full text-left p-1.5 rounded border border-border-subtle bg-surface-0 hover:border-brand-500/30 mb-1 last:mb-0 transition-colors"
        >
          <div className="flex items-center gap-1.5 min-w-0">
            <span className="text-xs text-text-primary font-mono truncate">%{tp.sourceVar}%</span>
            <span className="text-xs text-text-tertiary shrink-0">→</span>
            <span className="text-xs text-amber-400 truncate">{tp.sinkType}</span>
            {tp.path && tp.path.length > 2 && (
              <span className="ml-auto text-2xs text-text-tertiary shrink-0 font-mono">{tp.path.length} hops</span>
            )}
          </div>
        </button>
      ))}
      {hidden > 0 && (
        <button
          onClick={() => setShowAll(v => !v)}
          className="text-2xs text-brand-400 hover:text-brand-300 transition-colors mt-1"
        >
          {showAll ? 'Show fewer' : `Show ${hidden} more`}
        </button>
      )}
    </div>
  )
}

// DeadDataPanel lists each dead-data variable.
// Each row shows the variable, why it's dead, and jumps to the block that sets
// it so the developer can decide whether to remove the write.
function DeadDataPanel({
  paths,
  onNavigate,
}: {
  paths: NonNullable<DataFlowAnalysis['deadData']>
  onNavigate: (id: string) => void
}) {
  const [showAll, setShowAll] = React.useState(false)
  const INITIAL = 5
  const visible = showAll ? paths : paths.slice(0, INITIAL)
  const hidden = paths.length - visible.length

  return (
    <div className="p-3 rounded-lg border border-border-subtle bg-surface-0">
      <div className="flex items-center justify-between mb-1">
        <span className="text-xs font-bold text-text-tertiary">Dead Data ({paths.length})</span>
        <span className="text-2xs text-text-tertiary">set, only read by unreachable code</span>
      </div>
      <p className="text-2xs text-text-tertiary mb-2">
        Variables written but only consumed where execution can't reach.
      </p>
      {visible.map((dp, i) => (
        <button
          key={i}
          onClick={() => onNavigate(dp.setBlock)}
          className="block w-full text-left p-1.5 rounded border border-border-subtle bg-surface-2 hover:border-brand-500/30 mb-1 last:mb-0 transition-colors"
        >
          <div className="flex items-center gap-1.5 min-w-0">
            <span className="text-xs text-text-primary font-mono truncate">%{dp.variable}%</span>
            <span className="text-2xs text-text-tertiary ml-auto shrink-0 truncate">
              {dp.reason || 'unreachable reader'}
            </span>
          </div>
        </button>
      ))}
      {hidden > 0 && (
        <button
          onClick={() => setShowAll(v => !v)}
          className="text-2xs text-brand-400 hover:text-brand-300 transition-colors mt-1"
        >
          {showAll ? 'Show fewer' : `Show ${hidden} more`}
        </button>
      )}
    </div>
  )
}

export default function DataFlowInsights() {
  const doc = useFlowStore(s => s.document)
  const navigateToBlock = useFlowStore(s => s.navigateToBlock)
  const [dataFlow, setDataFlow] = React.useState<DataFlowAnalysis | null>(null)
  // Re-fetch when a new analysis lands, not just when the document changes —
  // otherwise these insights show the previous run's data after re-analyze.
  const generatedAt = useAnalysisStore(s => (doc ? s.reports.get(doc.id)?.generatedAt : undefined))

  React.useEffect(() => {
    if (!doc) return
    let cancelled = false
    analysisApi
      .getDataFlow()
      .then(r => {
        if (!cancelled) setDataFlow(r as DataFlowAnalysis)
      })
      .catch(err => {
        if (!cancelled) logger.warn('Failed to load dataflow analysis', err)
      })
    return () => {
      cancelled = true
    }
  }, [doc, generatedAt])

  if (
    !dataFlow ||
    ((!dataFlow.taintPaths || dataFlow.taintPaths.length === 0) &&
      (!dataFlow.deadData || dataFlow.deadData.length === 0))
  ) {
    return null
  }

  return (
    <div>
      <h3 className="text-xs font-bold uppercase tracking-widest text-text-tertiary mb-2 flex items-center gap-1.5">
        <ShieldAlert size={12} />
        Data Flow Insights
      </h3>
      <div className="space-y-2">
        {dataFlow.taintPaths && dataFlow.taintPaths.length > 0 && (
          <TaintPathsPanel paths={dataFlow.taintPaths} onNavigate={navigateToBlock} />
        )}
        {dataFlow.deadData && dataFlow.deadData.length > 0 && (
          <DeadDataPanel paths={dataFlow.deadData} onNavigate={navigateToBlock} />
        )}
      </div>
    </div>
  )
}
