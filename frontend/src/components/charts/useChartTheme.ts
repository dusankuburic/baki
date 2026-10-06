import type {ChartTheme} from './chartTypes'

// The chart palette as CSS variable references (index.css, "DATA VIZ"). SVG
// presentation attributes resolve `var()` like any other CSS value, so a chart
// never re-renders for a theme or high-contrast change: the browser restyles
// it together with the rest of the page. (The colours used to be read from the
// computed style as hex once per theme, which re-rendered every chart on the
// page on each switch — about 350ms on the analytics page.)
const token = (name: string) => `var(${name})`

// Typed to match ChartTheme's fixed-length tuples — the palette really is
// exactly 8 categorical + 4 ramp slots (index.css, "DATA VIZ").
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
