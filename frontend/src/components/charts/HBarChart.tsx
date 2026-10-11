import {BarChart, Bar, XAxis, YAxis, Tooltip, CartesianGrid, Cell, LabelList} from 'recharts'
import {useChartTheme} from './useChartTheme'
import {useReducedMotion} from './useReducedMotion'
import {ChartFigure} from './ChartFigure'
import {ChartTooltip} from './ChartTooltip'
import {ChartLegend} from './ChartLegend'
import {formatCompactIntl} from '@/lib/format'
import {
  HBAR_MARGIN,
  BAR_MAX_SIZE,
  BAR_RADIUS_RIGHT,
  barChartHeight,
  gridProps,
  xAxisNumber,
  yAxisCategory,
  cursorBar,
} from './axes'

export interface HBarRow {
  key: string
  label: string
  value: number
}

export interface HBarSeries {
  key: string
  name: string
  color: string
}

export function HBarChart<Row extends HBarRow>({
  rows,
  valueName,
  series,
  valueFormatter = formatCompactIntl,
  allowDecimals = false,
  color,
  rowColor,
  activeKey,
  onBarClick,
  maxLabel = 22,
  minHeight,
}: {
  rows: Row[]
  valueName: string
  series?: HBarSeries[]
  valueFormatter?: (n: number) => string
  allowDecimals?: boolean
  color?: string
  rowColor?: (row: Row) => string
  activeKey?: string | null
  onBarClick?: (key: string) => void
  maxLabel?: number
  minHeight?: number
}) {
  const t = useChartTheme()
  const reduce = useReducedMotion()
  const fill = color ?? t.emphasis
  const clickable = !!onBarClick
  const handleClick = clickable
    ? (_: unknown, index: number) => {
        const row = rows[index]
        if (row) onBarClick?.(row.key)
      }
    : undefined

  return (
    <>
      <ChartFigure height={barChartHeight(rows.length, minHeight)}>
        <BarChart data={rows} layout="vertical" margin={HBAR_MARGIN} barCategoryGap={3}>
          <CartesianGrid {...gridProps(t)} horizontal={false} vertical />
          <XAxis {...xAxisNumber(t, valueFormatter, allowDecimals)} />
          <YAxis {...yAxisCategory(t, 'label', maxLabel)} />
          <Tooltip cursor={cursorBar(t)} content={<ChartTooltip theme={t} valueFormatter={v => valueFormatter(v)} />} />
          {series ? (
            series.map((s, i) => (
              <Bar
                key={s.key}
                dataKey={s.key}
                name={s.name}
                stackId="stack"
                fill={s.color}
                stroke={t.surface}
                strokeWidth={1}
                radius={i === series.length - 1 ? BAR_RADIUS_RIGHT : undefined}
                maxBarSize={BAR_MAX_SIZE}
                isAnimationActive={!reduce}
                onClick={handleClick}
                cursor={clickable ? 'pointer' : undefined}
              />
            ))
          ) : (
            <Bar
              dataKey="value"
              name={valueName}
              fill={fill}
              radius={BAR_RADIUS_RIGHT}
              maxBarSize={BAR_MAX_SIZE}
              isAnimationActive={!reduce}
              onClick={handleClick}
              cursor={clickable ? 'pointer' : undefined}
            >
              {rows.map(r => (
                <Cell
                  key={r.key}
                  fill={rowColor ? rowColor(r) : fill}
                  fillOpacity={activeKey && activeKey !== r.key ? 0.35 : 1}
                />
              ))}
              <LabelList
                dataKey="value"
                position="right"
                formatter={(v: unknown) => valueFormatter(Number(v))}
                fill={t.text.secondary}
                fontSize={11}
              />
            </Bar>
          )}
        </BarChart>
      </ChartFigure>
      {series && (
        <ChartLegend
          items={series.map(s => ({key: s.key, label: s.name, color: s.color, shape: 'rect'}))}
          className="mt-2"
        />
      )}
    </>
  )
}
