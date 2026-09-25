import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {ChartCard, ColumnChart, HBarChart, type HBarRow} from '@/components/charts'
import {formatDateLong} from '@/lib/format'
import {relativeTimeText, absoluteTime} from '@/lib/time'
import type {DashboardAnalyticsData} from '@/types'
import {actionIcon, actionLabel} from '../../home/activity'
import {ChartSection} from '../ChartSection'

// Who did what in this scope: events per day, the mix of actions, and the
// latest events with their actor.
export function ActivitySection({data, refetching}: {data: DashboardAnalyticsData; refetching: boolean}) {
  const {t} = useTranslation('dashboard')
  const labels = t('actions', {returnObjects: true}) as Record<string, string>
  const activity = data.activity
  const failed = data.partial.includes('activity')
  const hasDays = activity.byDay.some(d => d.count > 0)
  const actionRows = useMemo<HBarRow[]>(
    () => activity.byAction.map(a => ({key: a.action, label: actionLabel(labels, a.action), value: a.count})),
    [activity.byAction, labels],
  )
  const state = (ok: boolean) => (failed ? 'error' : ok ? 'ready' : 'empty')

  return (
    <ChartSection
      id="analytics-activity"
      title={t('analytics.sections.activity')}
      description={t('analytics.sectionSub.activity')}
    >
      <ChartCard
        title={t('analytics.cards.activityPerDay')}
        state={state(hasDays)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.activityEmpty')}
        className="analytics-span-7"
        table={{
          caption: t('analytics.cards.activityPerDay'),
          filename: 'activity-per-day.csv',
          columns: [
            {key: 'date', header: t('analytics.columns.date'), render: r => formatDateLong(r.date)},
            {key: 'count', header: t('analytics.columns.count'), align: 'right'},
          ],
          rows: activity.byDay,
        }}
      >
        <ColumnChart data={activity.byDay} dataKey="count" name={t('analytics.columns.count')} height={200} />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.activityByAction')}
        state={state(actionRows.length > 0)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.activityEmpty')}
        className="analytics-span-5"
        table={{
          caption: t('analytics.cards.activityByAction'),
          filename: 'activity-by-action.csv',
          columns: [
            {key: 'label', header: t('analytics.columns.action')},
            {key: 'key', header: t('analytics.columns.actionId')},
            {key: 'value', header: t('analytics.columns.count'), align: 'right'},
          ],
          rows: actionRows,
        }}
      >
        <HBarChart rows={actionRows} valueName={t('analytics.columns.count')} maxLabel={26} />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.activityFeed')}
        state={state(activity.events.length > 0)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.activityEmpty')}
        className="analytics-span-12"
      >
        <ul className="max-h-80 divide-y divide-border-subtle/60 overflow-y-auto pr-1 scrollbar-thin">
          {activity.events.map((e, i) => {
            const Icon = actionIcon(e.action)
            return (
              <li key={`${e.action}-${e.createdAt}-${i}`} className="flex items-center gap-3 py-1.5 text-xs">
                <Icon size={13} className="shrink-0 text-text-tertiary" aria-hidden="true" />
                <span className="w-28 shrink-0 truncate font-medium text-text-primary">
                  {e.actor.isMe ? t('common.you') : e.actor.name || '—'}
                </span>
                <span className="min-w-0 flex-1 truncate">
                  <span className="text-text-primary">{actionLabel(labels, e.action)}</span>
                  {e.label && <span className="ml-1 text-text-tertiary">· {e.label}</span>}
                </span>
                <span title={absoluteTime(e.createdAt)} className="shrink-0 text-2xs text-text-tertiary">
                  {relativeTimeText(e.createdAt)}
                </span>
              </li>
            )
          })}
        </ul>
      </ChartCard>
    </ChartSection>
  )
}
