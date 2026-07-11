import {useFlowStore} from '@/stores/flowStore'
import {useAnalysisStore} from '@/stores/analysisStore'
import {csvCell, downloadBlob} from '@/lib/csv'
import {formatCount} from '@/lib/format'
import {scoreColor, scoreBg, scoreLabel} from '@/lib/scoring'
import {BarChart3, RefreshCw, Download} from 'lucide-react'
import clsx from 'clsx'
import type {FlowMetrics} from '@/types'
import {ComplexityScatter, ImpactEffortMatrix} from './ComplexityCharts'
import StatCard from './StatCard'
import SubflowMetricsRow from './SubflowMetricsRow'
import HealthTrend from './HealthTrend'
import DataFlowInsights from './DataFlowInsights'

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
        <span className="text-xs text-text-disabled">Click "Run Analysis" in the Findings tab</span>
      </div>
    )
  }

  return (
    <div className="flex flex-col h-full overflow-y-auto custom-scrollbar">
      <div className="p-4 space-y-4">
        <div className={clsx('p-4 rounded-xl border', scoreBg(metrics.healthScore))}>
          <div className="flex items-center justify-between mb-1">
            <span className="text-xs font-bold uppercase tracking-widest text-text-tertiary flex items-center gap-2">
              Health Score
              <button
                onClick={() => exportMetricsCSV(metrics, doc.id)}
                title="Export metrics as CSV"
                aria-label="Export metrics as CSV"
                className="text-text-tertiary hover:text-text-secondary p-0.5 rounded hover:bg-surface-3 transition-colors"
              >
                <Download size={12} />
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
          <StatCard label="Subflows" value={formatCount(metrics.subflowCount)} />
          <StatCard label="Total Blocks" value={formatCount(metrics.totalBlocks)} />
          <StatCard label="Total Variables" value={formatCount(metrics.totalVariables)} />
          <StatCard label="Avg Cyclomatic" value={metrics.avgCyclomatic.toFixed(1)} />
          <StatCard label="Max Cyclomatic" value={metrics.maxCyclomatic} warn={metrics.maxCyclomatic > 20} />
          <StatCard label="Max Cognitive" value={metrics.maxCognitive} warn={metrics.maxCognitive > 30} />
        </div>

        {metrics.circularDependencies && metrics.circularDependencies.length > 0 && (
          <div className="p-3 rounded-lg border border-red-500/20 bg-red-500/5">
            <div className="flex items-center gap-2 mb-1">
              <RefreshCw size={12} className="text-red-400" />
              <span className="text-xs font-bold text-red-400">Circular Dependencies</span>
            </div>
            <p className="text-xs text-text-secondary">
              Subflow call cycle detected: {metrics.circularDependencies.join(' → ')}
            </p>
          </div>
        )}

        <DataFlowInsights />

        {report?.findings && report.findings.length > 0 && (
          <ImpactEffortMatrix findings={report.findings} />
        )}

        {metrics.subflows.length > 0 && (
          <ComplexityScatter subflows={metrics.subflows} />
        )}

        {ruleProfiles && ruleProfiles.length > 0 && (
          <div>
            <h3 className="text-xs font-bold uppercase tracking-widest text-text-tertiary mb-2">Rule Performance</h3>
            <div className="space-y-1">
              {ruleProfiles
                .filter(rp => rp.durationMs > 0)
                .sort((a, b) => b.durationMs - a.durationMs)
                .slice(0, 8)
                .map(rp => (
                  <div key={rp.ruleId} className="flex items-center gap-2 px-2 py-1.5 rounded border border-border-subtle bg-surface-0">
                    <span className="text-xs text-text-primary flex-1 truncate">{rp.ruleName}</span>
                    <span className="text-xs text-text-tertiary tabular-nums">{formatCount(rp.findingCount)} findings</span>
                    <span className={clsx('text-xs font-mono tabular-nums', rp.durationMs > 50 ? 'text-amber-400' : 'text-text-tertiary')}>
                      {rp.durationMs}ms
                    </span>
                  </div>
                ))}
            </div>
          </div>
        )}

        <div>
          <h3 className="text-xs font-bold uppercase tracking-widest text-text-tertiary mb-2">Per-Subflow Complexity</h3>
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
