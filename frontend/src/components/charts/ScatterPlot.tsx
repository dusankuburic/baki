import {ScatterChart, Scatter, XAxis, YAxis, ZAxis, Tooltip, CartesianGrid} from 'recharts'
import {useChartTheme} from './useChartTheme'
import {useReducedMotion} from './useReducedMotion'
import {ChartFigure} from './ChartFigure'
import {ChartTooltip} from './ChartTooltip'
import {ChartLegend} from './ChartLegend'
import {formatCompactIntl} from '@/lib/format'
import {CHART_MARGIN, gridProps, xAxisNumber, yAxisNumber, cursorLine} from './axes'

export interface ScatterPoint {
  key: string
  label: string
  x: number
  y: number
  /** The group (legend entry) the point belongs to. */
  group: string
}

export interface ScatterGroup {
  key: string
  label: string
  color: string
}

// Points in groups (health tiers). Markers are 8px with a 2px surface ring so
// overlaps stay countable; the tooltip names the point and both axes.
export function ScatterPlot({
  points,
  groups,
  xName,
  yName,
  xFormatter = formatCompactIntl,
  yFormatter = formatCompactIntl,
  height = 220,
}: {
  points: ScatterPoint[]
  groups: ScatterGroup[]
  xName: string
  yName: string
  xFormatter?: (n: number) => string
  yFormatter?: (n: number) => string
  height?: number
}) {
  const t = useChartTheme()
  const reduce = useReducedMotion()
  return (
    <>
      <ChartFigure height={height}>
        <ScatterChart margin={CHART_MARGIN}>
          <CartesianGrid {...gridProps(t)} vertical />
          <XAxis {...xAxisNumber(t, xFormatter)} dataKey="x" name={xName} />
          <YAxis {...yAxisNumber(t, yFormatter)} dataKey="y" name={yName} />
          <ZAxis range={[64, 64]} />
          <Tooltip
            cursor={cursorLine(t)}
            content={
              <ChartTooltip
                theme={t}
                labelFormatter={(_label, payload) => String(payload[0]?.payload?.label ?? '')}
                valueFormatter={(v, name) => (name === xName ? xFormatter(v) : yFormatter(v))}
              />
            }
          />
          {groups.map(g => (
            <Scatter
              key={g.key}
              name={g.label}
              data={points.filter(p => p.group === g.key)}
              fill={g.color}
              stroke={t.surface}
              strokeWidth={2}
              isAnimationActive={!reduce}
            />
          ))}
        </ScatterChart>
      </ChartFigure>
      <ChartLegend
        items={groups.map(g => ({key: g.key, label: g.label, color: g.color, shape: 'dot'}))}
        className="mt-2"
      />
    </>
  )
}
