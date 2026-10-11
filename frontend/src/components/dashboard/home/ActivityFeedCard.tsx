import {useTranslation} from 'react-i18next'
import {ChartCard} from '@/components/charts/ChartCard'
import {relativeTimeText, absoluteTime} from '@/lib/time'
import {DetailsAction} from './DetailsAction'
import {actionIcon, actionLabel} from './activity'
import type {ActivityEntry} from '@/types'

export function ActivityFeedCard({
  data,
  onDetails,
  className,
}: {
  data: ActivityEntry[]
  onDetails?: () => void
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const labels = t('actions', {returnObjects: true}) as Record<string, string>
  const title = t('home.activity')
  return (
    <ChartCard
      title={title}
      actions={<DetailsAction title={title} onClick={onDetails} />}
      state={data.length > 0 ? 'ready' : 'empty'}
      emptyMessage={t('home.activityEmpty')}
      className={className}
    >
      <ul className="max-h-64 space-y-0.5 overflow-y-auto pr-1 scrollbar-thin">
        {data.slice(0, 15).map((entry, i) => {
          const Icon = actionIcon(entry.action)
          return (
            <li
              key={`${entry.action}-${entry.createdAt}-${i}`}
              className="flex items-center gap-2.5 rounded-md px-1 py-1 transition-colors duration-fast hover:bg-surface-3/40"
            >
              <Icon size={13} className="shrink-0 text-text-tertiary" aria-hidden="true" />
              <div className="min-w-0 flex-1 truncate text-xs">
                <span className="text-text-primary">{actionLabel(labels, entry.action)}</span>
                {entry.flowName && <span className="ml-1 text-text-tertiary">· {entry.flowName}</span>}
              </div>
              <span title={absoluteTime(entry.createdAt)} className="shrink-0 text-2xs text-text-tertiary">
                {relativeTimeText(entry.createdAt)}
              </span>
            </li>
          )
        })}
      </ul>
    </ChartCard>
  )
}
