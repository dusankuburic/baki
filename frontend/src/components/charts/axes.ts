import type {DataKey} from 'recharts'
import type {ChartTheme} from './chartTypes'
import {formatCompactIntl, formatDateTick} from '@/lib/format'

// Axis and mark presets shared by every chart, so the quiet look — hairline
// solid grid, recessive axes, thin marks, no clipped tick — is fixed once.
//
// The y-axis is sized by Recharts ("auto") to its widest formatted tick and the
// left margin is 0: the old cards used a negative left margin with a fixed
// narrow width, which pushed "100" out of the svg and left "0" on screen.

export const CHART_MARGIN = {top: 8, right: 12, bottom: 0, left: 0} as const
export const BAR_MAX_SIZE = 24
export const BAR_RADIUS_UP: [number, number, number, number] = [4, 4, 0, 0]
export const BAR_RADIUS_RIGHT: [number, number, number, number] = [0, 4, 4, 0]
export const AREA_FILL_OPACITY = 0.1
export const LINE_WIDTH = 2
export const MARKER_RADIUS = 4
// Height of the x-axis band (ticks + labels) a chart container must include.
export const AXIS_BAND = 28

// A horizontal bar chart grows with its rows so every label renders: 22px per
// row (a bar of at most 24px plus its surface gap) plus the x-axis band.
export function barChartHeight(rows: number, min = 96): number {
  return Math.max(min, rows * 22 + AXIS_BAND + 8)
}

export function tickStyle(t: ChartTheme) {
  return {fill: t.tick, fontSize: 11, fontFamily: t.fontSans}
}

export function gridProps(t: ChartTheme) {
  return {stroke: t.grid, vertical: false} as const
}

export function xAxisDate(t: ChartTheme) {
  return {
    dataKey: 'date',
    axisLine: {stroke: t.axis},
    tickLine: false,
    tick: tickStyle(t),
    tickMargin: 8,
    minTickGap: 24,
    tickFormatter: (d: string) => formatDateTick(d),
  }
}

export function xAxisCategory(t: ChartTheme, dataKey: string) {
  return {
    dataKey,
    axisLine: {stroke: t.axis},
    tickLine: false,
    tick: tickStyle(t),
    tickMargin: 8,
    interval: 0 as const,
  }
}

export function xAxisNumber(t: ChartTheme, format: (n: number) => string = formatCompactIntl, allowDecimals = false) {
  return {
    type: 'number' as const,
    axisLine: {stroke: t.axis},
    tickLine: false,
    tick: tickStyle(t),
    tickMargin: 8,
    tickFormatter: format,
    allowDecimals,
  }
}

export function yAxisNumber(
  t: ChartTheme,
  format: (n: number) => string = formatCompactIntl,
  domain?: [number | string, number | string],
  allowDecimals = false,
) {
  return {
    axisLine: false,
    tickLine: false,
    tick: tickStyle(t),
    tickMargin: 8,
    width: 'auto' as const,
    tickFormatter: format,
    allowDecimals,
    ...(domain ? {domain} : {}),
  }
}

// Horizontal bars carry their value at the tip; the margin keeps a label on
// a bar that reaches the domain's end inside the svg.
export const HBAR_MARGIN = {...CHART_MARGIN, right: 40} as const

export function yAxisCategory(t: ChartTheme, dataKey: string, maxLabel = 22) {
  return {
    type: 'category' as const,
    dataKey,
    axisLine: false,
    tickLine: false,
    tick: tickStyle(t),
    tickMargin: 8,
    width: 'auto' as const,
    // Every row keeps its label; the height grows with the rows instead.
    interval: 0 as const,
    tickFormatter: (s: string) => truncateLabel(s, maxLabel),
  }
}

// Recharts cannot resolve its TypedDataKey for a row type that is still
// generic inside a reusable chart; the caller's key is checked against the row
// (`keyof Row & string`) and handed over here.
export function dataKeyOf<Row>(key: keyof Row & string): DataKey<Row> {
  return key as unknown as DataKey<Row>
}

export function truncateLabel(s: string, max = 22): string {
  return s.length > max ? `${s.slice(0, max - 1)}…` : s
}

// Tooltip cursors: a hairline for time charts, a faint wash for bars.
export function cursorLine(t: ChartTheme) {
  return {stroke: t.crosshair, strokeWidth: 1}
}

export function cursorBar(t: ChartTheme) {
  return {fill: t.text.tertiary, fillOpacity: 0.08}
}
