import {ComposedChart, Area, Line, XAxis, YAxis, Tooltip, CartesianGrid, ReferenceLine} from 'recharts'
import {useChartTheme} from './useChartTheme'
import {useReducedMotion} from './useReducedMotion'
import {ChartFigure} from './ChartFigure'
import {ChartTooltip} from './ChartTooltip'
import {ChartLegend} from './ChartLegend'
import {formatCompactIntl} from '@/lib/format'
import {
  CHART_MARGIN,
  LINE_WIDTH,
  MARKER_RADIUS,
  AREA_FILL_OPACITY,
  gridProps,
  xAxisDate,
  yAxisNumber,
  cursorLine,
  dataKeyOf,
} from './axes'

export function LineTrendChart<Row extends {date: string}>({
  data,
  dataKey,
  name,
  prevKey,
  prevName,
  domain,
  referenceLines = [],
  valueFormatter = formatCompactIntl,
  height = 220,
}: {
  data: Row[]
  dataKey: keyof Row & string
  name: string
  prevKey?: keyof Row & string
  prevName?: string
  domain?: [number | string, number | string]
  referenceLines?: number[]
  valueFormatter?: (n: number) => string
  height?: number
}) {
  const t = useChartTheme()
  const reduce = useReducedMotion()
  const points = data.filter(d => d[dataKey] !== null && d[dataKey] !== undefined).length
  const showPrev = !!prevKey && data.some(d => d[prevKey] !== null && d[prevKey] !== undefined)
  return (
    <>
      <ChartFigure height={height}>
        <ComposedChart data={data} margin={CHART_MARGIN}>
          <CartesianGrid {...gridProps(t)} />
          <XAxis {...xAxisDate(t)} />
          <YAxis {...yAxisNumber(t, valueFormatter, domain)} />
          {referenceLines.map(v => (
            <ReferenceLine key={v} y={v} stroke={t.axis} strokeWidth={1} strokeOpacity={0.7} ifOverflow="hidden" />
          ))}
          <Tooltip
            cursor={cursorLine(t)}
            content={<ChartTooltip theme={t} valueFormatter={v => valueFormatter(v)} />}
          />
          {showPrev && prevKey && (
            <Line
              type="monotone"
              dataKey={dataKeyOf<Row>(prevKey)}
              name={prevName}
              stroke={t.muted}
              strokeWidth={1.5}
              dot={false}
              activeDot={{r: 3, stroke: t.surface, strokeWidth: 2}}
              connectNulls={false}
              isAnimationActive={!reduce}
            />
          )}
          <Area
            type="monotone"
            dataKey={dataKeyOf<Row>(dataKey)}
            name={name}
            stroke={t.emphasis}
            strokeWidth={LINE_WIDTH}
            fill={t.emphasis}
            fillOpacity={AREA_FILL_OPACITY}
            dot={points === 1 ? {r: MARKER_RADIUS, fill: t.emphasis, stroke: t.surface, strokeWidth: 2} : false}
            activeDot={{r: MARKER_RADIUS, stroke: t.surface, strokeWidth: 2}}
            connectNulls={false}
            isAnimationActive={!reduce}
          />
        </ComposedChart>
      </ChartFigure>
      {showPrev && prevName && (
        <ChartLegend
          items={[
            {key: 'current', label: name, color: t.emphasis, shape: 'line'},
            {key: 'prev', label: prevName, color: t.muted, shape: 'line'},
          ]}
          className="mt-2"
        />
      )}
    </>
  )
}
