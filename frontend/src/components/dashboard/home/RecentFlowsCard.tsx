import clsx from 'clsx'
import {FileText} from 'lucide-react'
import {CardShell, CardPlaceholder} from './CardShell'
import type {RecentFlowStub} from '@/types'
import {useUIStore} from '@/stores/uiStore'

// RecentFlowsCard lists the most recently updated flows with their latest health
// score (or “—” when never analyzed). Rows open the flow.
export function RecentFlowsCard({
  flows,
  onOpen,
  className,
}: {
  flows: RecentFlowStub[]
  onOpen: (id: string) => void
  className?: string
}) {
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const browseAction = (
    <button
      type="button"
      onClick={() => setMainPaneView('library')}
      className="text-2xs font-medium text-brand-400 hover:text-brand-300 transition-colors"
    >
      Browse all →
    </button>
  )
  return (
    <CardShell title="Recent Flows" action={browseAction} className={className}>
      {flows.length === 0 ? (
        <CardPlaceholder message="No flows yet. Create or open one to get started." />
      ) : (
        <ul className="flex flex-col divide-y divide-border-subtle/60">
          {flows.map(f => (
            <li key={f.id}>
              <button
                onClick={() => onOpen(f.id)}
                className="w-full flex items-center gap-3 py-2 px-1 text-left rounded-md hover:bg-surface-3/60 transition-colors"
              >
                <FileText size={15} className="text-text-tertiary shrink-0" />
                <span className="flex-1 truncate text-sm text-text-primary">{f.name || 'Untitled flow'}</span>
                <span className="text-2xs text-text-tertiary tabular-nums shrink-0">{relativeTime(f.updatedAt)}</span>
                <HealthBadge score={f.healthScore} />
              </button>
            </li>
          ))}
        </ul>
      )}
    </CardShell>
  )
}

function HealthBadge({score}: {score: number | null}) {
  if (score == null) {
    return <span className="w-9 text-center text-2xs font-mono text-text-tertiary shrink-0">—</span>
  }
  const tone = score >= 80 ? 'text-emerald-400 bg-emerald-500/10'
    : score >= 50 ? 'text-amber-400 bg-amber-500/10'
    : 'text-red-400 bg-red-500/10'
  return (
    <span className={clsx('w-9 text-center text-2xs font-mono font-semibold rounded px-1 py-0.5 shrink-0', tone)}>
      {score}
    </span>
  )
}

function relativeTime(iso: string): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ''
  const secs = Math.max(0, (Date.now() - then) / 1000)
  if (secs < 60) return 'now'
  const mins = secs / 60
  if (mins < 60) return `${Math.floor(mins)}m`
  const hrs = mins / 60
  if (hrs < 24) return `${Math.floor(hrs)}h`
  const days = hrs / 24
  if (days < 7) return `${Math.floor(days)}d`
  return `${Math.floor(days / 7)}w`
}
