import type {ChartTheme} from './chartTypes'

const token = (name: string) => `var(${name})`

const SERIES: ChartTheme['series'] = [
  token('--viz-1'),
  token('--viz-2'),
  token('--viz-3'),
  token('--viz-4'),
  token('--viz-5'),
  token('--viz-6'),
  token('--viz-7'),
  token('--viz-8'),
]
const SEQ: ChartTheme['seq'] = [token('--viz-seq-1'), token('--viz-seq-2'), token('--viz-seq-3'), token('--viz-seq-4')]

export const CHART_THEME: ChartTheme = Object.freeze({
  series: Object.freeze(SERIES),
  seq: Object.freeze(SEQ),
  status: {success: token('--success'), warning: token('--warning'), error: token('--error'), info: token('--info')},
  emphasis: token('--viz-emphasis'),
  muted: token('--viz-muted'),
  grid: token('--viz-grid'),
  axis: token('--viz-axis'),
  tick: token('--viz-tick'),
  crosshair: token('--viz-crosshair'),
  text: {primary: token('--text-primary'), secondary: token('--text-secondary'), tertiary: token('--text-tertiary')},
  surface: token('--surface-2'),
  tooltip: {bg: token('--viz-tooltip-bg'), border: token('--viz-tooltip-border')},
  fontSans: token('--font-sans'),
  fontMono: token('--font-mono'),
})

export function useChartTheme(): ChartTheme {
  return CHART_THEME
}
