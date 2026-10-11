import type {ReactNode} from 'react'

export interface ChartTheme {
  series: readonly [string, string, string, string, string, string, string, string]
  seq: readonly [string, string, string, string]
  status: {success: string; warning: string; error: string; info: string}
  emphasis: string
  muted: string
  grid: string
  axis: string
  tick: string
  crosshair: string
  text: {primary: string; secondary: string; tertiary: string}
  surface: string
  tooltip: {bg: string; border: string}
  fontSans: string
  fontMono: string
}

export interface LegendItem {
  key: string
  label: string
  color: string
  shape?: 'rect' | 'line' | 'dot'
}

export interface TableColumn<Row> {
  key: string
  header: string
  align?: 'left' | 'right'
  render?: (row: Row) => ReactNode
  csv?: (row: Row) => string
}

export interface TableSpec<Row> {
  caption: string
  columns: TableColumn<Row>[]
  rows: Row[]
  filename?: string
  rowKey: (row: Row, index: number) => string
}
