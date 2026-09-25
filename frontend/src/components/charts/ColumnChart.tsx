import {BarChart, Bar, XAxis, YAxis, Tooltip, CartesianGrid} from 'recharts'
import {useChartTheme} from './useChartTheme'
import {useReducedMotion} from './useReducedMotion'
import {ChartFigure} from './ChartFigure'
import {ChartTooltip} from './ChartTooltip'
import {formatCompactIntl} from '@/lib/format'
import {
  CHART_MARGIN,
  BAR_MAX_SIZE,
  BAR_RADIUS_UP,
  gridProps,
  xAxisDate,
  yAxisNumber,
  cursorBar,
  dataKeyOf,
} from './axes'

// One value per day as columns in a single hue: counts of things that
// happened (runs, resolutions, events, calls, spend).
export function ColumnChart<Row extends {date: string}>({
  data,
  dataKey,
  name,
  valueFormatter = formatCompactIntl,
  allowDecimals = false,
  color,
  height = 180,
}: {
  data: Row[]
  dataKey: keyof Row & string
  name: string
  valueFormatter?: (n: number) => string
  /** Fractional axis ticks (currency). */
  allowDecimals?: boolean
  color?: string
  height?: number
}) {
  const t = useChartTheme()
  const reduce = useReducedMotion()
  return (
    <ChartFigure height={height}>
      <BarChart data={data} margin={CHART_MARGIN} barCategoryGap="20%">
        <CartesianGrid {...gridProps(t)} />
        <XAxis {...xAxisDate(t)} />
        <YAxis {...yAxisNumber(t, valueFormatter, undefined, allowDecimals)} />
        <Tooltip cursor={cursorBar(t)} content={<ChartTooltip theme={t} valueFormatter={v => valueFormatter(v)} />} />
        <Bar
          dataKey={dataKeyOf<Row>(dataKey)}
          name={name}
          fill={color ?? t.emphasis}
          radius={BAR_RADIUS_UP}
          maxBarSize={BAR_MAX_SIZE}
          isAnimationActive={!reduce}
        />
      </BarChart>
    </ChartFigure>
  )
}
