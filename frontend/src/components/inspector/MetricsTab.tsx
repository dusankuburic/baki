import React from 'react'
import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'
import {csvCell, downloadBlob} from '@/lib/csv'
import {scoreColor, scoreBg, scoreLabel} from '@/lib/scoring'
import {BarChart3, RefreshCw, ArrowDownToLine, ArrowUpFromLine, ShieldAlert, Download, TrendingUp} from 'lucide-react'
import clsx from 'clsx'
import type {AnalysisSnapshot, FlowMetrics, SubflowMetrics, DataFlowAnalysis, TaintPath} from '@/types/domain'

function exportMetricsCSV(metrics: FlowMetrics, flowId: string) {
  const rows = [
    ['Subflow', 'Blocks', 'Cyclomatic', 'Cognitive', 'Fan-In', 'Fan-Out', 'Max Depth', 'Variables'],
    ...metrics.subflows.map(m => [
      csvCell(m.subflowName),
      m.blockCount, m.cyclomaticComplexity, m.cognitiveComplexity,
      m.fanIn, m.fanOut, m.maxNestingDepth, m.variableCount,
    ].map(String)),
    [],
    ['Health Score', String(metrics.healthScore)],
    ['Total Blocks', String(metrics.totalBlocks)],
    ['Total Variables', String(metrics.totalVariables)],
    ['Avg Cyclomatic', metrics.avgCyclomatic.toFixed(1)],
  ]
  const csv = rows.map(r => r.join(',')).join('\n')
  downloadBlob(csv, 'text/csv;charset=utf-8;', `metrics-${flowId}-${new Date().toISOString().slice(0, 10)}.csv`)
}

function MiniBar({value, max, color}: {value: number; max: number; color: string}) {
  const pct = max > 0 ? Math.min(100, (value / max) * 100) : 0
  return (
    <div className="h-1.5 w-full bg-surface-3 rounded-full overflow-hidden">
      <div className={clsx('h-full rounded-full transition-all duration-fast', color)} style={{width: `${pct}%`}} />
    </div>
  )
}

const SubflowMetricsRow = React.memo(function SubflowMetricsRow({m, onSelect}: {m: SubflowMetrics; onSelect: () => void}) {
  const cycloColor = m.cyclomaticComplexity > 20 ? 'bg-red-500' : m.cyclomaticComplexity > 10 ? 'bg-amber-500' : 'bg-green-500'
  const cogColor = m.cognitiveComplexity > 30 ? 'bg-red-500' : m.cognitiveComplexity > 15 ? 'bg-amber-500' : 'bg-green-500'

  return (
    <button
      onClick={onSelect}
      className="w-full text-left p-3 rounded-lg border border-border-subtle bg-surface-0 hover:border-brand-500/30 hover:bg-surface-2 transition-all duration-fast"
    >
      <div className="flex items-center justify-between mb-2">
        <span className="text-xs font-medium text-text-primary truncate">{m.subflowName}</span>
        <span className="text-2xs text-text-tertiary tabular-nums">{m.blockCount} blocks</span>
      </div>
      <div className="grid grid-cols-2 gap-x-4 gap-y-1.5">
        <div>
          <div className="flex items-center justify-between mb-0.5">
            <span className="text-2xs text-text-tertiary uppercase tracking-wider">Cyclomatic</span>
            <span className="text-2xs font-mono text-text-secondary tabular-nums">{m.cyclomaticComplexity}</span>
          </div>
          <MiniBar value={m.cyclomaticComplexity} max={30} color={cycloColor} />
        </div>
        <div>
          <div className="flex items-center justify-between mb-0.5">
            <span className="text-2xs text-text-tertiary uppercase tracking-wider">Cognitive</span>
            <span className="text-2xs font-mono text-text-secondary tabular-nums">{m.cognitiveComplexity}</span>
          </div>
          <MiniBar value={m.cognitiveComplexity} max={40} color={cogColor} />
        </div>
      </div>
      <div className="flex items-center gap-3 mt-2 pt-2 border-t border-border-subtle">
        <span className="flex items-center gap-1 text-2xs text-text-tertiary">
          <ArrowDownToLine size={9} /> Fan-in: {m.fanIn}
        </span>
        <span className="flex items-center gap-1 text-2xs text-text-tertiary">
          <ArrowUpFromLine size={9} /> Fan-out: {m.fanOut}
        </span>
        <span className="flex items-center gap-1 text-2xs text-text-tertiary">
          Depth: {m.maxNestingDepth}
        </span>
        <span className="flex items-center gap-1 text-2xs text-text-tertiary">
          Vars: {m.variableCount}
        </span>
      </div>
    </button>
  )
})

export default function MetricsTab() {
  const doc = useFlowStore(s => s.document)
  const report = useAnalysisStore(s => doc ? s.reports.get(doc.id) : undefined)
  const selectSubflow = useFlowStore(s => s.selectSubflow)

  const metrics = report?.metrics
  const ruleProfiles = report?.ruleProfiles

  if (!doc) {
    return (
      <div className="flex items-center justify-center h-full text-sm text-text-tertiary p-4 text-center">
        Load a flow to view metrics
      </div>
    )
  }

  if (!metrics) {
    return (
      <div className="flex flex-col items-center justify-center h-full gap-3 p-4">
        <BarChart3 size={24} className="text-text-tertiary" />
        <span className="text-sm text-text-tertiary">Run analysis to see metrics</span>
        <span className="text-2xs text-text-disabled">Click "Run Analysis" in the Findings tab</span>
      </div>
    )
  }

  const hasCircular = metrics.circularDependencies && metrics.circularDependencies.length > 0

  return (
    <div className="flex flex-col h-full overflow-y-auto custom-scrollbar">
      <div className="p-4 space-y-4">
        <div className={clsx('p-4 rounded-xl border', scoreBg(metrics.healthScore))}>
          <div className="flex items-center justify-between mb-1">
            <span className="text-2xs font-bold uppercase tracking-widest text-text-tertiary flex items-center gap-2">
              Health Score
              <button
                onClick={() => exportMetricsCSV(metrics, doc.id)}
                title="Export metrics as CSV"
                aria-label="Export metrics as CSV"
                className="text-text-tertiary hover:text-text-secondary p-0.5 rounded hover:bg-surface-3 transition-colors"
              >
                <Download size={11} />
              </button>
            </span>
            <span className={clsx('text-2xl font-black font-mono tabular-nums', scoreColor(metrics.healthScore))}>
              {metrics.healthScore}
            </span>
          </div>
          <div className="h-2 w-full bg-surface-3 rounded-full overflow-hidden mb-2">
            <div
              className={clsx('h-full rounded-full transition-all duration-fast', scoreColor(metrics.healthScore).replace('text-', 'bg-'))}
              style={{width: `${metrics.healthScore}%`}}
            />
          </div>
          <span className={clsx('text-xs font-medium', scoreColor(metrics.healthScore))}>
            {scoreLabel(metrics.healthScore)}
          </span>
        </div>

        <HealthTrend />

        <div className="grid grid-cols-2 gap-2">
          <StatCard label="Subflows" value={metrics.subflowCount} />
          <StatCard label="Total Blocks" value={metrics.totalBlocks} />
          <StatCard label="Total Variables" value={metrics.totalVariables} />
          <StatCard label="Avg Cyclomatic" value={metrics.avgCyclomatic.toFixed(1)} />
          <StatCard label="Max Cyclomatic" value={metrics.maxCyclomatic} warn={metrics.maxCyclomatic > 20} />
          <StatCard label="Max Cognitive" value={metrics.maxCognitive} warn={metrics.maxCognitive > 30} />
        </div>

        {hasCircular && (
          <div className="p-3 rounded-lg border border-red-500/20 bg-red-500/5">
            <div className="flex items-center gap-2 mb-1">
              <RefreshCw size={12} className="text-red-400" />
              <span className="text-xs font-bold text-red-400">Circular Dependencies</span>
            </div>
            <p className="text-2xs text-text-secondary">
              Subflow call cycle detected: {metrics.circularDependencies!.join(' → ')}
            </p>
          </div>
        )}

        <DataFlowInsights />

        {ruleProfiles && ruleProfiles.length > 0 && (
          <div>
            <h3 className="text-2xs font-bold uppercase tracking-widest text-text-tertiary mb-2">Rule Performance</h3>
            <div className="space-y-1">
              {ruleProfiles
                .filter(rp => rp.durationMs > 0)
                .sort((a, b) => b.durationMs - a.durationMs)
                .slice(0, 8)
                .map(rp => (
                  <div key={rp.ruleId} className="flex items-center gap-2 px-2 py-1.5 rounded border border-border-subtle bg-surface-0">
                    <span className="text-2xs text-text-primary flex-1 truncate">{rp.ruleName}</span>
                    <span className="text-2xs text-text-tertiary tabular-nums">{rp.findingCount} findings</span>
                    <span className={clsx('text-2xs font-mono tabular-nums', rp.durationMs > 50 ? 'text-amber-400' : 'text-text-tertiary')}>
                      {rp.durationMs}ms
                    </span>
                  </div>
                ))}
            </div>
          </div>
        )}

        <div>
          <h3 className="text-2xs font-bold uppercase tracking-widest text-text-tertiary mb-2">Per-Subflow Complexity</h3>
          <div className="space-y-2">
            {metrics.subflows.map(sf => (
              <SubflowMetricsRow
                key={sf.subflowId}
                m={sf}
                onSelect={() => selectSubflow(sf.subflowId)}
              />
            ))}
          </div>
        </div>
      </div>
    </div>
  )
}

function StatCard({label, value, warn}: {label: string; value: string | number; warn?: boolean}) {
  return (
    <div className="p-2.5 rounded-lg border border-border-subtle bg-surface-0">
      <div className="text-2xs text-text-tertiary uppercase tracking-wider mb-0.5">{label}</div>
      <div className={clsx('text-sm font-bold font-mono tabular-nums', warn ? 'text-amber-400' : 'text-text-primary')}>
        {value}
      </div>
    </div>
  )
}

// HealthTrend renders the persisted analysis snapshots as a health-score
// sparkline with per-run severity counts on hover. Hidden until there are at
// least two distinct runs to connect.
function HealthTrend() {
  const doc = useFlowStore(s => s.document)
  const generatedAt = useAnalysisStore(s => doc ? s.reports.get(doc.id)?.generatedAt : undefined)
  const [snapshots, setSnapshots] = React.useState<AnalysisSnapshot[]>([])
  const [hover, setHover] = React.useState<number | null>(null)

  React.useEffect(() => {
    if (!doc) return
    let cancelled = false
    analysisApi.getHistory()
      .then(s => { if (!cancelled) setSnapshots((s ?? []).slice(-20)) })
      .catch((err) => { if (!cancelled) logger.warn('Failed to load history', err) })
    return () => { cancelled = true }
  }, [doc, generatedAt])

  if (snapshots.length < 2) return null

  const W = 280
  const H = 56
  const PAD = 6
  const step = (W - PAD * 2) / (snapshots.length - 1)
  const y = (score: number) => PAD + (H - PAD * 2) * (1 - Math.max(0, Math.min(100, score)) / 100)
  const points = snapshots.map((s, i) => `${PAD + i * step},${y(s.healthScore)}`).join(' ')
  const active = hover != null ? snapshots[hover] : snapshots[snapshots.length - 1]

  return (
    <div className="p-3 rounded-lg border border-border-subtle bg-surface-0">
      <div className="flex items-center justify-between mb-1">
        <span className="text-2xs font-bold uppercase tracking-widest text-text-tertiary flex items-center gap-1.5">
          <TrendingUp size={10} />
          Health Trend
        </span>
        <span className="text-2xs text-text-tertiary tabular-nums">
          {new Date(active.timestamp).toLocaleString()} · score {active.healthScore} ·{' '}
          <span className="text-red-400">{active.errors}E</span>{' '}
          <span className="text-amber-400">{active.warnings}W</span>{' '}
          <span className="text-blue-400">{active.info}I</span>
        </span>
      </div>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="w-full"
        role="img"
        aria-label={`Health score trend over ${snapshots.length} analysis runs`}
        onMouseLeave={() => setHover(null)}
      >
        <polyline points={points} fill="none" stroke="var(--brand-500)" strokeWidth="1.5" strokeLinejoin="round" />
        {snapshots.map((s, i) => (
          <g key={s.timestamp + i}>
            {/* invisible wide hit area per point */}
            <rect
              x={PAD + i * step - step / 2}
              y={0}
              width={step}
              height={H}
              fill="transparent"
              onMouseEnter={() => setHover(i)}
            />
            <circle
              cx={PAD + i * step}
              cy={y(s.healthScore)}
              r={hover === i ? 3.5 : 2}
              fill={s.errors > 0 ? 'var(--error)' : s.warnings > 0 ? 'var(--warning)' : 'var(--success)'}
              className="transition-all duration-fast"
            />
          </g>
        ))}
      </svg>
    </div>
  )
}

function DataFlowInsights() {
  const doc = useFlowStore(s => s.document)
  const navigateToBlock = useFlowStore(s => s.navigateToBlock)
  const [dataFlow, setDataFlow] = React.useState<DataFlowAnalysis | null>(null)
  // Re-fetch when a new analysis lands, not just when the document changes —
  // otherwise these insights show the previous run's data after re-analyze.
  const generatedAt = useAnalysisStore(s => doc ? s.reports.get(doc.id)?.generatedAt : undefined)

  React.useEffect(() => {
    if (!doc) return
    let cancelled = false
    analysisApi.getDataFlow()
      .then(r => { if (!cancelled) setDataFlow(r as DataFlowAnalysis) })
      .catch((err) => { if (!cancelled) logger.warn('Failed to load dataflow analysis', err) })
    return () => { cancelled = true }
  }, [doc, generatedAt])

  if (!dataFlow || ((!dataFlow.taintPaths || dataFlow.taintPaths.length === 0) && (!dataFlow.deadData || dataFlow.deadData.length === 0))) {
    return null
  }

  return (
    <div>
      <h3 className="text-2xs font-bold uppercase tracking-widest text-text-tertiary mb-2 flex items-center gap-1.5">
        <ShieldAlert size={10} />
        Data Flow Insights
      </h3>
      <div className="space-y-2">
        {dataFlow.taintPaths && dataFlow.taintPaths.length > 0 && (
          <div className="p-3 rounded-lg border border-amber-500/20 bg-amber-500/5">
            <div className="text-2xs font-bold text-amber-400 mb-1">Taint Paths ({dataFlow.taintPaths.length})</div>
            <p className="text-2xs text-text-secondary mb-2">
              User input flows to sensitive sinks without validation.
            </p>
            {dataFlow.taintPaths.slice(0, 5).map((tp: TaintPath, i: number) => (
              <button
                key={i}
                onClick={() => navigateToBlock(tp.sinkBlock)}
                className="block w-full text-left p-1.5 rounded border border-border-subtle bg-surface-0 hover:border-brand-500/30 mb-1 last:mb-0 transition-colors"
              >
                <span className="text-2xs text-text-primary font-mono">%{tp.sourceVar}%</span>
                <span className="text-2xs text-text-tertiary mx-1">→</span>
                <span className="text-2xs text-amber-400">{tp.sinkType}</span>
              </button>
            ))}
          </div>
        )}
        {dataFlow.deadData && dataFlow.deadData.length > 0 && (
          <div className="p-3 rounded-lg border border-border-subtle bg-surface-0">
            <div className="text-2xs font-bold text-text-tertiary mb-1">Dead Data Paths ({dataFlow.deadData.length})</div>
            <p className="text-2xs text-text-secondary">
              Variables set but only consumed by unreachable blocks.
            </p>
          </div>
        )}
      </div>
    </div>
  )
}
