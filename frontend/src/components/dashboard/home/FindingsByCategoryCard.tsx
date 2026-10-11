import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {ChartCard, HBarChart, type HBarRow} from '@/components/charts'
import {DetailsAction} from './DetailsAction'
import type {DashboardFindingsAgg} from '@/types'

export function FindingsByCategoryCard({
  findings,
  onDetails,
  className,
}: {
  findings: DashboardFindingsAgg
  onDetails?: () => void
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const rows = useMemo<HBarRow[]>(
    () =>
      [...findings.byCategory]
        .filter(c => c.count > 0)
        .sort((a, b) => b.count - a.count)
        .map(c => ({key: c.category, label: c.category, value: c.count})),
    [findings.byCategory],
  )
  const title = t('home.byCategory')
  return (
    <ChartCard
      title={title}
      actions={<DetailsAction title={title} onClick={onDetails} />}
      state={rows.length > 0 ? 'ready' : 'empty'}
      emptyMessage={t('home.byCategoryEmpty')}
      className={className}
      table={{
        caption: title,
        filename: 'findings-by-category.csv',
        rowKey: r => r.key,
        columns: [
          {key: 'label', header: t('analytics.columns.category')},
          {key: 'value', header: t('analytics.columns.findings'), align: 'right'},
        ],
        rows,
      }}
    >
      <HBarChart rows={rows} valueName={t('analytics.columns.findings')} minHeight={150} />
    </ChartCard>
  )
}
