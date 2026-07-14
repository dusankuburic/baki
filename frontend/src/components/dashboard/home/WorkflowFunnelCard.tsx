import {Clock, AlertTriangle} from 'lucide-react'
import {CardShell, CardPlaceholder} from './CardShell'
import {useChartColors, type ChartColors} from './useChartColors'
import type {Workflow} from '@/types'

// Status display order and colors. These aren't a strict subset funnel
// (findings jump between states), so we render the distribution as per-status
// bars rather than a recharts FunnelChart, which would imply each stage is a
// subset of the previous. Colors come from useChartColors so they re-resolve
// on theme toggle like every other card.
const STAGES: {key: string; label: string; color: keyof ChartColors}[] = [
  {key: 'open', label: 'Open', color: 'textSecondary'},
  {key: 'acknowledged', label: 'Acknowledged', color: 'info'},
  {key: 'in_progress', label: 'In Progress', color: 'warning'},
  {key: 'resolved', label: 'Resolved', color: 'success'},
  {key: 'suppressed', label: 'Suppressed', color: 'surface3'},
]

function formatDuration(hours: number): string {
  if (hours <= 0) return '—'
  if (hours < 1) return `${Math.round(hours * 60)}m`
  if (hours < 24) return `${hours.toFixed(1)}h`
  const days = Math.floor(hours / 24)
  const remH = Math.round(hours % 24)
  return remH > 0 ? `${days}d ${remH}h` : `${days}d`
}

// WorkflowFunnelCard shows how the team is working through findings: the
// status distribution, mean time to resolve, and how many findings have gone
// stale. Cloud-only — local mode has no persistent triage, so the card shows a
// placeholder pointing at the findings triage actions.
export function WorkflowFunnelCard({data, className}: {data: Workflow; className?: string}) {
  const colors = useChartColors()
  if (!data?.available) {
    return (
      <CardShell title="Triage Workflow" className={className}>
        <CardPlaceholder message="Triage findings (open → resolved) to see your team's workflow health." />
      </CardShell>
    )
  }

  const funnel = data.funnel ?? {}
  const total = STAGES.reduce((s, st) => s + (funnel[st.key] ?? 0), 0)
  const maxCount = Math.max(1, ...STAGES.map(st => funnel[st.key] ?? 0))

  return (
    <CardShell title="Triage Workflow" className={className}>
      <div className="flex flex-col h-full">
        {/* Status distribution */}
        <div className="space-y-1.5 mb-3">
          {STAGES.map(st => {
            const n = funnel[st.key] ?? 0
            const pct = total > 0 ? Math.round((n / total) * 100) : 0
            return (
              <div key={st.key} className="flex items-center gap-2">
                <span className="text-2xs text-text-tertiary w-20 shrink-0">{st.label}</span>
                <div className="flex-1 h-4 bg-surface-3/60 rounded overflow-hidden">
                  <div
                    className="h-full rounded transition-all"
                    style={{width: `${(n / maxCount) * 100}%`, backgroundColor: colors[st.color]}}
                  />
                </div>
                <span className="text-2xs font-mono tabular-nums text-text-secondary w-14 text-right shrink-0">
                  {n} ({pct}%)
                </span>
              </div>
            )
          })}
        </div>

        {/* Resolution health */}
        <div className="grid grid-cols-2 gap-2 mt-auto">
          <div className="flex items-center gap-2 rounded-lg bg-surface-3/50 px-2.5 py-2">
            <Clock size={15} className="text-brand-400 shrink-0" />
            <div className="min-w-0">
              <div className="text-sm font-mono tabular-nums text-text-primary leading-tight">
                {data.mttrHours > 0 ? formatDuration(data.mttrHours) : '—'}
              </div>
              <div className="text-2xs text-text-tertiary leading-tight">MTTR · {data.resolvedCount} resolved</div>
            </div>
          </div>
          <div
            className="flex items-center gap-2 rounded-lg px-2.5 py-2"
            style={{backgroundColor: data.staleCount > 0 ? 'rgba(239,68,68,0.08)' : 'rgba(63,63,71,0.4)'}}
          >
            <AlertTriangle
              size={15}
              className={data.staleCount > 0 ? 'text-red-400 shrink-0' : 'text-text-tertiary shrink-0'}
            />
            <div className="min-w-0">
              <div
                className={`text-sm font-mono tabular-nums leading-tight ${data.staleCount > 0 ? 'text-red-400' : 'text-text-primary'}`}
              >
                {data.staleCount}
              </div>
              <div className="text-2xs text-text-tertiary leading-tight">stale (&gt;14d open)</div>
            </div>
          </div>
        </div>
      </div>
    </CardShell>
  )
}
