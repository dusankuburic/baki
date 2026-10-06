import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {ChartCard, LineTrendChart, ColumnChart, StackedAreaChart, useChartTheme} from '@/components/charts'
import {formatDateLong} from '@/lib/format'
import type {DashboardAnalyticsData} from '@/types'
import {ChartSection} from '../ChartSection'

interface Row {
  date: string
  avgHealth: number | null
  prevAvgHealth: number | null
  prevDate: string
  flowCount: number
  runs: number
  findings: number
  errors: number
  warnings: number
  info: number
}

// Day-by-day series for the window: health (with the previous period aligned
// day for day), how many analyses ran, and the open findings by severity.
export function TrendSection({data, refetching}: {data: DashboardAnalyticsData; refetching: boolean}) {
  const {t} = useTranslation('dashboard')
  const theme = useChartTheme()
  const compare = data.applied.compare
  const rows = useMemo<Row[]>(
    () =>
      data.trend.points.map((p, i) => {
        const prev = data.trend.prevPoints[i]
        return {
          date: p.date,
          avgHealth: p.avgHealth,
          prevAvgHealth: compare ? (prev?.avgHealth ?? null) : null,
          prevDate: prev?.date ?? '',
          flowCount: p.flowCount,
          runs: p.runs,
          findings: p.findings,
          errors: p.errors,
          warnings: p.warnings,
          info: p.info,
        }
      }),
    [data.trend, compare],
  )
  const hasHealth = rows.some(r => r.avgHealth !== null)
  const hasRuns = rows.some(r => r.runs > 0)
  const hasFindings = rows.some(r => r.findings > 0)
  const failed = data.partial.includes('trend')
  const state = (ok: boolean) => (failed ? 'error' : ok ? 'ready' : 'empty')
  const dateCol = {key: 'date', header: t('analytics.columns.date'), render: (r: Row) => formatDateLong(r.date)}

  return (
    <ChartSection
      id="analytics-trend"
      title={t('analytics.sections.trend')}
      description={t('analytics.sectionSub.trend')}
    >
      <ChartCard
        title={t('analytics.cards.healthTrend')}
        subtitle={t('analytics.cards.healthTrendSub')}
        state={state(hasHealth)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.healthTrendEmpty')}
        className="analytics-span-8"
        table={{
          caption: t('analytics.cards.healthTrend'),
          filename: 'health-trend.csv',
          rowKey: r => r.date,
          columns: [
            dateCol,
            {
              key: 'avgHealth',
              header: t('analytics.columns.avgHealth'),
              align: 'right',
              render: r => (r.avgHealth === null ? '—' : r.avgHealth),
              csv: r => (r.avgHealth === null ? '' : String(r.avgHealth)),
            },
            ...(compare
              ? [
                  {
                    key: 'prevAvgHealth',
                    header: t('analytics.columns.prevAvgHealth'),
                    align: 'right' as const,
                    render: (r: Row) => (r.prevAvgHealth === null ? '—' : r.prevAvgHealth),
                    csv: (r: Row) => (r.prevAvgHealth === null ? '' : String(r.prevAvgHealth)),
                  },
                ]
              : []),
            {key: 'flowCount', header: t('analytics.columns.flows'), align: 'right'},
          ],
          rows,
        }}
      >
        <LineTrendChart
          data={rows}
          dataKey="avgHealth"
          name={t('analytics.columns.avgHealth')}
          prevKey={compare ? 'prevAvgHealth' : undefined}
          prevName={t('analytics.columns.prevAvgHealth')}
          domain={[0, 100]}
          referenceLines={[80, 60, 40]}
          height={240}
        />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.runs')}
        subtitle={t('analytics.cards.runsSub')}
        state={state(hasRuns)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.runsEmpty')}
        className="analytics-span-4"
        table={{
          caption: t('analytics.cards.runs'),
          filename: 'analysis-runs.csv',
          rowKey: r => r.date,
          columns: [dateCol, {key: 'runs', header: t('analytics.columns.runs'), align: 'right'}],
          rows,
        }}
      >
        <ColumnChart data={rows} dataKey="runs" name={t('analytics.columns.runs')} height={240} />
      </ChartCard>
      <ChartCard
        title={t('analytics.cards.severityTrend')}
        subtitle={t('analytics.cards.severityTrendSub')}
        state={state(hasFindings)}
        refetching={refetching}
        emptyMessage={t('analytics.cards.severityTrendEmpty')}
        className="analytics-span-12"
        table={{
          caption: t('analytics.cards.severityTrend'),
          filename: 'findings-by-severity.csv',
          rowKey: r => r.date,
          columns: [
            dateCol,
            {key: 'errors', header: t('analytics.columns.errors'), align: 'right'},
            {key: 'warnings', header: t('analytics.columns.warnings'), align: 'right'},
            {key: 'info', header: t('analytics.columns.info'), align: 'right'},
            {key: 'findings', header: t('analytics.columns.findings'), align: 'right'},
          ],
          rows,
        }}
      >
        <StackedAreaChart
          data={rows}
          series={[
            {key: 'errors', name: t('severityPlural.error'), color: theme.status.error},
            {key: 'warnings', name: t('severityPlural.warning'), color: theme.status.warning},
            {key: 'info', name: t('severityPlural.info'), color: theme.status.info},
          ]}
          height={220}
        />
      </ChartCard>
    </ChartSection>
  )
}
