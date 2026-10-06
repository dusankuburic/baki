import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {ChartCard, ScatterPlot, useChartTheme, type ScatterPoint, type ScatterGroup} from '@/components/charts'
import {healthTier, type HealthTier} from '@/lib/scoring'
import {DetailsAction} from './DetailsAction'

export interface ComplexityRow {
  id: string
  name: string
  blocks: number
  findings: number
  health: number
}

// Blocks against findings, one point per flow, coloured by health tier. The
// same figure serves Home (session or cloud) and the Analytics page.
export function FlowComplexityCard({
  rows,
  title,
  subtitle,
  onDetails,
  refetching,
  className,
}: {
  rows: ComplexityRow[]
  title?: string
  subtitle?: string
  onDetails?: () => void
  refetching?: boolean
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const theme = useChartTheme()
  const heading = title ?? t('home.complexity')
  const points = useMemo<ScatterPoint[]>(
    () =>
      rows
        .filter(r => r.blocks > 0)
        .map(r => ({key: r.id, label: r.name, x: r.blocks, y: r.findings, group: healthTier(r.health).tier})),
    [rows],
  )
  const groups = useMemo<ScatterGroup[]>(() => {
    const colour: Record<HealthTier, string> = {
      good: theme.status.success,
      fair: theme.status.warning,
      poor: theme.status.error,
      critical: theme.status.error,
    }
    return (['good', 'fair', 'poor'] as const).map(tier => ({
      key: tier,
      label: t(`health.${tier}`),
      color: colour[tier],
    }))
  }, [theme, t])
  // Critical shares the poor colour; fold it into that legend entry.
  const folded = useMemo(() => points.map(p => (p.group === 'critical' ? {...p, group: 'poor'} : p)), [points])
  const tableRows = useMemo(() => rows.map(r => ({...r, tier: t(`health.${healthTier(r.health).tier}`)})), [rows, t])
  return (
    <ChartCard
      title={heading}
      subtitle={subtitle ?? t('home.complexitySub')}
      actions={<DetailsAction title={heading} onClick={onDetails} />}
      state={points.length > 0 ? 'ready' : 'empty'}
      emptyMessage={t('home.complexityEmpty')}
      refetching={refetching}
      className={className}
      table={{
        caption: heading,
        filename: 'flow-complexity.csv',
        rowKey: r => r.id,
        columns: [
          {key: 'name', header: t('analytics.columns.flow')},
          {key: 'blocks', header: t('home.blocks'), align: 'right'},
          {key: 'findings', header: t('home.findings'), align: 'right'},
          {key: 'health', header: t('analytics.columns.health'), align: 'right'},
          {key: 'tier', header: t('analytics.columns.status')},
        ],
        rows: tableRows,
      }}
    >
      <ScatterPlot points={folded} groups={groups} xName={t('home.blocks')} yName={t('home.findings')} height={220} />
    </ChartCard>
  )
}
