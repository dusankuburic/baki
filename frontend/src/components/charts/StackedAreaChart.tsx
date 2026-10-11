import {AreaChart, Area, XAxis, YAxis, Tooltip, CartesianGrid} from 'recharts'
import {useChartTheme} from './useChartTheme'
import {useReducedMotion} from './useReducedMotion'
import {ChartFigure} from './ChartFigure'
import {ChartTooltip} from './ChartTooltip'
import {ChartLegend} from './ChartLegend'
import {formatCompactIntl} from '@/lib/format'
import {CHART_MARGIN, LINE_WIDTH, MARKER_RADIUS, gridProps, xAxisDate, yAxisNumber, cursorLine} from './axes'

export interface AreaSeries {
  key: string
  name: string
  color: string
}

export function StackedAreaChart<Row extends {date: string}>({
  data,
  series,
  valueFormatter = formatCompactIntl,
  height = 200,
  fillOpacity = 0.35,
}: {
  data: Row[]
  series: AreaSeries[]
  valueFormatter?: (n: number) => string
  height?: number
  fillOpacity?: number
}) {
  const t = useChartTheme()
  const reduce = useReducedMotion()
  return (
    <>
      <ChartFigure height={height}>
        <AreaChart data={data} margin={CHART_MARGIN}>
          <CartesianGrid {...gridProps(t)} />
          <XAxis {...xAxisDate(t)} />
          <YAxis {...yAxisNumber(t, valueFormatter)} />
          <Tooltip
            cursor={cursorLine(t)}
            content={<ChartTooltip theme={t} valueFormatter={v => valueFormatter(v)} />}
          />
          {series.map(s => (
            <Area
              key={s.key}
              type="monotone"
              dataKey={s.key}
              name={s.name}
              stackId="stack"
              stroke={s.color}
              strokeWidth={LINE_WIDTH}
              fill={s.color}
              fillOpacity={fillOpacity}
              dot={false}
              activeDot={{r: MARKER_RADIUS, stroke: t.surface, strokeWidth: 2}}
              isAnimationActive={!reduce}
            />
          ))}
        </AreaChart>
      </ChartFigure>
      <ChartLegend
        items={series.map(s => ({key: s.key, label: s.name, color: s.color, shape: 'rect'}))}
        className="mt-2"
      />
    </>
  )
}
