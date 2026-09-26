import type {ReactNode} from 'react'

// The chart palette as CSS colour values — `var(--token)` references that SVG
// presentation attributes resolve like any stylesheet, so charts follow a theme
// change without re-rendering (see useChartTheme).
export interface ChartTheme {
  /** Categorical slots 1–8 in the validated fixed order: assign in sequence, never cycle. */
  series: readonly string[]
  /** Ordinal one-hue ramp, light → dark, for ordered categories (funnel stages, tiers). */
  seq: readonly string[]
  /** Reserved for state: severity and health tiers. Never a series colour. */
  status: {success: string; warning: string; error: string; info: string}
  /** The one series a single-series chart draws. */
  emphasis: string
  /** De-emphasis: context lines, "Other", sparkline history. */
  muted: string
  grid: string
  axis: string
  tick: string
  crosshair: string
  text: {primary: string; secondary: string; tertiary: string}
  /** The card surface charts render on (surface gaps and marker rings use it). */
  surface: string
  tooltip: {bg: string; border: string}
  fontSans: string
  fontMono: string
}

export interface LegendItem {
  key: string
  label: string
  color: string
  /** Mirrors the mark: rect for bars/areas, line for lines, dot for markers. */
  shape?: 'rect' | 'line' | 'dot'
}

export interface TableColumn<Row> {
  key: string
  header: string
  align?: 'left' | 'right'
  /** Cell content; defaults to the row's value for `key`. */
  render?: (row: Row) => ReactNode
  /** CSV text; defaults to the row's value for `key`. */
  csv?: (row: Row) => string
}

export interface TableSpec<Row> {
  caption: string
  columns: TableColumn<Row>[]
  rows: Row[]
  /** Offer a CSV download under this file name. */
  filename?: string
}
