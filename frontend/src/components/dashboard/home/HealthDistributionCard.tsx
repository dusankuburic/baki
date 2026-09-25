import {useMemo} from 'react'
import {useTranslation} from 'react-i18next'
import {BarChart, Bar, XAxis, YAxis, Tooltip, CartesianGrid} from 'recharts'
import {
  ChartCard,
  ChartFigure,
  ChartTooltip,
  useChartTheme,
  useReducedMotion,
  CHART_MARGIN,
  BAR_MAX_SIZE,
  BAR_RADIUS_UP,
  gridProps,
  xAxisCategory,
  yAxisNumber,
  cursorBar,
} from '@/components/charts'
import {DetailsAction} from './DetailsAction'
import type {HealthBucket} from '@/types'

// How the analyzed flows spread across the five score bands: a single-hue
// histogram (the bands are ordered, the bars are counts of flows).
export function HealthDistributionCard({
  data,
  title,
  subtitle,
  onDetails,
  refetching,
  className,
}: {
  data: HealthBucket[]
  title?: string
  subtitle?: string
  onDetails?: () => void
  refetching?: boolean
  className?: string
}) {
  const {t} = useTranslation('dashboard')
  const theme = useChartTheme()
  const reduce = useReducedMotion()
  const rows = useMemo(() => data.map(b => ({...b, band: b.label})), [data])
  const hasData = rows.some(b => b.count > 0)
  const heading = title ?? t('home.healthDistribution')
  return (
    <ChartCard
      title={heading}
      subtitle={subtitle ?? t('home.healthDistributionSub')}
      actions={<DetailsAction title={heading} onClick={onDetails} />}
      state={hasData ? 'ready' : 'empty'}
      refetching={refetching}
      emptyMessage={t('home.healthDistributionEmpty')}
      className={className}
      table={{
        caption: heading,
        filename: 'health-distribution.csv',
        columns: [
          {key: 'band', header: t('analytics.columns.band')},
          {key: 'count', header: t('analytics.columns.flows'), align: 'right'},
        ],
        rows,
      }}
    >
      <ChartFigure height={180}>
        <BarChart data={rows} margin={CHART_MARGIN} barCategoryGap="25%">
          <CartesianGrid {...gridProps(theme)} />
          <XAxis {...xAxisCategory(theme, 'band')} />
          <YAxis {...yAxisNumber(theme)} />
          <Tooltip cursor={cursorBar(theme)} content={<ChartTooltip theme={theme} />} />
          <Bar
            dataKey="count"
            name={t('home.flowsInBand')}
            fill={theme.emphasis}
            radius={BAR_RADIUS_UP}
            maxBarSize={BAR_MAX_SIZE * 2}
            isAnimationActive={!reduce}
          />
        </BarChart>
      </ChartFigure>
    </ChartCard>
  )
}
