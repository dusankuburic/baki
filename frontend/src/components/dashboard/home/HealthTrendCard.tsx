import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {ChartCard, LineTrendChart} from '@/components/charts'
import {formatDateLong} from '@/lib/format'
import {DetailsAction} from './DetailsAction'
import type {DailyHealthPoint} from '@/types'

interface Row {
  date: string
  avgHealth: number | null
  flowCount: number
}

// Average health over the last 30 days. A day with no analyzed flow is a gap
// in the line, never a zero; hairlines mark the tier thresholds.
export function HealthTrendCard({
  data,
  onDetails,
  className,
}: {
  data: DailyHealthPoint[]
  onDetails?: () => void
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const rows = useMemo<Row[]>(
    () => data.map(d => ({date: d.date, avgHealth: d.flowCount > 0 ? d.avgHealth : null, flowCount: d.flowCount})),
    [data],
  )
  const hasData = rows.some(r => r.avgHealth !== null)
  const title = t('home.healthTrend')
  return (
    <ChartCard
      title={title}
      subtitle={t('home.healthTrendSub')}
      actions={<DetailsAction title={title} onClick={onDetails} />}
      state={hasData ? 'ready' : 'empty'}
      emptyMessage={t('home.healthTrendEmpty')}
      className={className}
      table={{
        caption: title,
        filename: 'health-trend.csv',
        rowKey: r => r.date,
        columns: [
          {key: 'date', header: t('analytics.columns.date'), render: r => formatDateLong(r.date)},
          {
            key: 'avgHealth',
            header: t('analytics.columns.avgHealth'),
            align: 'right',
            render: r => (r.avgHealth === null ? '—' : r.avgHealth),
            csv: r => (r.avgHealth === null ? '' : String(r.avgHealth)),
          },
          {key: 'flowCount', header: t('analytics.columns.flows'), align: 'right'},
        ],
        rows,
      }}
    >
      <LineTrendChart
        data={rows}
        dataKey="avgHealth"
        name={t('analytics.columns.avgHealth')}
        domain={[0, 100]}
        referenceLines={[80, 60, 40]}
        height={200}
      />
    </ChartCard>
  )
}
