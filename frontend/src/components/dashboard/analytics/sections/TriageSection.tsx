import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {Clock, Inbox, AlertTriangle} from 'lucide-react'
import {ChartCard, ColumnChart, HBarChart, StatTile, useChartTheme, type HBarRow} from '@/components/charts'
import {formatPercent, formatDateLong} from '@/lib/format'
import type {DashboardAnalyticsData} from '@/types'
import {TRIAGE_STATUSES, formatDuration} from '../../home/TriageCard'
import {ChartSection} from '../ChartSection'

interface Row extends HBarRow {
  share: number
}

// How the team is working through findings: the status distribution on the
// ordinal ramp (suppressed de-emphasised), resolutions per day, and the
// three numbers that say whether the backlog is moving.
export function TriageSection({data, refetching}: {data: DashboardAnalyticsData; refetching: boolean}) {
  const {t} = useTranslation('dashboard')
  const theme = useChartTheme()
  const triage = data.triage
  const failed = data.partial.includes('triage')
  const total = TRIAGE_STATUSES.reduce((s, k) => s + (triage.funnel[k] ?? 0), 0)
  const rows = useMemo<Row[]>(
    () =>
      TRIAGE_STATUSES.map(k => ({
        key: k,
        label: t(`triage.${k}`),
        value: triage.funnel[k] ?? 0,
        share: total > 0 ? (triage.funnel[k] ?? 0) / total : 0,
      })),
    [triage.funnel, total, t],
  )
  const ramp: Record<string, string> = {
    open: theme.seq[0],
    acknowledged: theme.seq[1],
    in_progress: theme.seq[2],
    resolved: theme.seq[3],
    suppressed: theme.muted,
  }
  const resolvedAny = triage.resolvedByDay.some(d => d.count > 0)
  const state = (ok: boolean) => (failed ? 'error' : ok ? 'ready' : 'empty')

  return (
    <ChartSection
      id="analytics-triage"
      title={t('analytics.sections.triage')}
      description={t('analytics.sectionSub.triage')}
    >
      <ChartCard
        title={t('analytics.cards.triageStatus')}
        subtitle={t('analytics.cards.triageStatusSub')}
        state={state(triage.available)}
        refetching={refetching}
        emptyMessage={t('triage.empty')}
        className="analytics-span-5"
        table={{
          caption: t('analytics.cards.triageStatus'),
          filename: 'triage-status.csv',
          rowKey: r => r.key,
          columns: [
            {key: 'label', header: t('triage.status')},
            {key: 'value', header: t('triage.findings'), align: 'right'},
            {
              key: 'share',
              header: t('triage.share'),
              align: 'right',
              render: r => formatPercent(r.share),
              csv: r => formatPercent(r.share),
            },
          ],
          rows,
        }}
      >
        <HBarChart
          rows={rows}
          valueName={t('triage.findings')}
          rowColor={r => ramp[r.key] ?? theme.emphasis}
          minHeight={150}
        />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.resolvedPerDay')}
        state={state(resolvedAny)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.resolvedEmpty')}
        className="analytics-span-7"
        table={{
          caption: t('analytics.cards.resolvedPerDay'),
          filename: 'resolved-per-day.csv',
          rowKey: r => r.date,
          columns: [
            {key: 'date', header: t('analytics.columns.date'), render: r => formatDateLong(r.date)},
            {key: 'count', header: t('triage.resolved'), align: 'right'},
          ],
          rows: triage.resolvedByDay,
        }}
      >
        <ColumnChart
          data={triage.resolvedByDay}
          dataKey="count"
          name={t('triage.resolved')}
          color={theme.seq[3]}
          height={200}
        />
      </ChartCard>
      <div className="analytics-tiles analytics-span-12">
        <StatTile
          label={t('triage.mttr')}
          value={formatDuration(triage.mttrHours)}
          sub={triage.resolvedCount > 0 ? t('triage.mttrSub', {count: triage.resolvedCount}) : t('triage.mttrNone')}
          icon={Clock}
          unavailable={failed ? t('tile.unavailable') : undefined}
        />
        <StatTile
          label={t('triage.backlog')}
          value={String(triage.backlog)}
          sub={t('triage.backlogSub')}
          icon={Inbox}
          unavailable={failed ? t('tile.unavailable') : undefined}
        />
        <StatTile
          label={t('triage.stale')}
          value={String(triage.staleCount)}
          sub={t('triage.staleSub')}
          icon={AlertTriangle}
          tone={triage.staleCount > 0 ? 'text-semantic-error' : undefined}
          unavailable={failed ? t('tile.unavailable') : undefined}
        />
      </div>
    </ChartSection>
  )
}
