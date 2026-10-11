import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {FileText, ArrowRight} from 'lucide-react'
import {ChartCard} from '@/components/charts/ChartCard'
import {healthTier} from '@/lib/scoring'
import {relativeTimeText, absoluteTime} from '@/lib/time'
import {useUIStore} from '@/stores/uiStore'
import type {RecentFlowStub} from '@/types'

export function RecentFlowsCard({
  flows,
  onOpen,
  className,
}: {
  flows: RecentFlowStub[]
  onOpen: (id: string) => void
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const title = t('home.recentFlows')
  return (
    <ChartCard
      title={title}
      actions={
        <button
          type="button"
          onClick={() => setMainPaneView('library')}
          className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-2xs font-medium text-brand-400 transition-colors duration-fast hover:text-brand-300 focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40"
        >
          {t('common.browseAll')}
          <ArrowRight size={11} aria-hidden="true" />
        </button>
      }
      state={flows.length > 0 ? 'ready' : 'empty'}
      emptyMessage={t('home.recentFlowsEmpty')}
      className={className}
    >
      <ul className="flex flex-col divide-y divide-border-subtle/60">
        {flows.map(f => (
          <li key={f.id}>
            <button
              type="button"
              onClick={() => onOpen(f.id)}
              className="flex w-full items-center gap-3 rounded-md px-1 py-2 text-left transition-colors duration-fast hover:bg-surface-3/60 focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/40"
            >
              <FileText size={15} className="shrink-0 text-text-tertiary" aria-hidden="true" />
              <span title={f.name || t('home.untitled')} className="flex-1 truncate text-sm text-text-primary">
                {f.name || t('home.untitled')}
              </span>
              <span title={absoluteTime(f.updatedAt)} className="shrink-0 text-2xs text-text-tertiary">
                {relativeTimeText(f.updatedAt)}
              </span>
              <HealthBadge score={f.healthScore} />
            </button>
          </li>
        ))}
      </ul>
    </ChartCard>
  )
}

export function HealthBadge({score}: {score: number | null}) {
  const {t} = useTranslation('dashboard')
  if (score == null) {
    return (
      <span className="w-9 shrink-0 text-center text-2xs text-text-tertiary" title={t('common.notAnalyzed')}>
        —
      </span>
    )
  }
  const tier = healthTier(score)
  return (
    <span
      className={clsx(
        'w-9 shrink-0 rounded px-1 py-0.5 text-center text-2xs font-semibold tabular-nums',
        tier.bg,
        tier.text,
      )}
      title={t(`health.${tier.tier}`)}
    >
      {score}
    </span>
  )
}
