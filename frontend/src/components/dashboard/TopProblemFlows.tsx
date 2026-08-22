import {FileWarning} from 'lucide-react'
import clsx from 'clsx'
import {formatCount} from '@/lib/format'
import {scoreColor} from '@/lib/scoring'
import type {DashboardStats} from '@/types'

// TopProblemFlows renders the worst-health flows from the aggregate stats.
// Extracted from AnalyticsDashboard; pure/presentational.
export function TopProblemFlows({flows}: {flows: NonNullable<DashboardStats['topProblemFlows']>}) {
  if (!flows || flows.length === 0) return null
  return (
    <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
      <h3 className="text-sm font-bold uppercase tracking-widest text-text-tertiary mb-2 flex items-center gap-1.5">
        <FileWarning size={14} />
        Top Problem Flows
      </h3>
      <div className="space-y-1">
        {flows.map(p => (
          <div
            key={p.flowId}
            className="flex items-center gap-2 px-2 py-1.5 rounded border border-border-subtle bg-surface-1"
          >
            <span className="text-sm text-text-primary flex-1 truncate">{p.flowName || p.flowId.slice(0, 8)}</span>
            <span className="text-sm text-text-tertiary tabular-nums">{formatCount(p.findingCount)} findings</span>
            <span className={clsx('text-sm font-bold font-mono tabular-nums', scoreColor(p.healthScore))}>
              {p.healthScore}
            </span>
          </div>
        ))}
      </div>
    </div>
  )
}
