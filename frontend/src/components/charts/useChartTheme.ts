import type {ChartTheme} from './chartTypes'

// The chart palette as CSS variable references (index.css, "DATA VIZ"). SVG
// presentation attributes resolve `var()` like any other CSS value, so a chart
// never re-renders for a theme or high-contrast change: the browser restyles
// it together with the rest of the page. (The colours used to be read from the
// computed style as hex once per theme, which re-rendered every chart on the
// page on each switch — about 350ms on the analytics page.)
const token = (name: string) => `var(${name})`

export const CHART_THEME: ChartTheme = Object.freeze({
  series: Object.freeze(Array.from({length: 8}, (_, i) => token(`--viz-${i + 1}`))),
  seq: Object.freeze(Array.from({length: 4}, (_, i) => token(`--viz-seq-${i + 1}`))),
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
