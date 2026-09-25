import {useMemo} from 'react'
import {useUIStore} from '@/stores/uiStore'
import {useSettingsStore} from '@/stores/settingsStore'
import {THEME_REGISTRY} from '@/lib/themeRegistry'
import type {ChartTheme} from './chartTypes'

// The chart tokens (`--viz-*`, index.css "DATA VIZ") are read from the computed
// style as hex once per theme, and again when high contrast toggles (it retints
// the grid and ticks). HTML parts of a chart — legend, tooltip, table — keep
// using the CSS variables directly; only the SVG needs resolved colours.

// The :root (Deep Dark) values, used before the DOM exists (SSR, jsdom).
const DARK_FALLBACK: ChartTheme = {
  series: ['#3987e5', '#d95926', '#199e70', '#c98500', '#d55181', '#008300', '#9085e9', '#e66767'],
  seq: ['#6da7ec', '#3987e5', '#256abf', '#184f95'],
  status: {success: '#22c55e', warning: '#eab308', error: '#ef4444', info: '#3b82f6'},
  emphasis: '#5b61ef',
  muted: '#6b6b76',
  grid: '#2a2a31',
  axis: '#3f3f47',
  tick: '#8e8e99',
  crosshair: '#3f3f47',
  text: {primary: '#f4f4f5', secondary: '#a1a1aa', tertiary: '#8e8e99'},
  surface: '#1c1c21',
  tooltip: {bg: '#131316', border: '#2a2a31'},
  fontSans: "'Inter', 'Geist', ui-sans-serif, system-ui, sans-serif",
  fontMono: "'JetBrains Mono', 'SF Mono', Menlo, Consolas, monospace",
  isDark: true,
  highContrast: false,
}

function reader(): ((name: string, fallback: string) => string) | null {
  if (typeof window === 'undefined' || typeof getComputedStyle !== 'function') return null
  const style = getComputedStyle(document.documentElement)
  return (name, fallback) => style.getPropertyValue(name).trim() || fallback
}

export function readChartTheme(themeId: string, highContrast: boolean): ChartTheme {
  const read = reader()
  const f = DARK_FALLBACK
  if (!read) return {...f, highContrast}
  return {
    series: f.series.map((c, i) => read(`--viz-${i + 1}`, c)),
    seq: f.seq.map((c, i) => read(`--viz-seq-${i + 1}`, c)),
    status: {
      success: read('--success', f.status.success),
      warning: read('--warning', f.status.warning),
      error: read('--error', f.status.error),
      info: read('--info', f.status.info),
    },
    emphasis: read('--viz-emphasis', f.emphasis),
    muted: read('--viz-muted', f.muted),
    grid: read('--viz-grid', f.grid),
    axis: read('--viz-axis', f.axis),
    tick: read('--viz-tick', f.tick),
    crosshair: read('--viz-crosshair', f.crosshair),
    text: {
      primary: read('--text-primary', f.text.primary),
      secondary: read('--text-secondary', f.text.secondary),
      tertiary: read('--text-tertiary', f.text.tertiary),
    },
    surface: read('--surface-2', f.surface),
    tooltip: {bg: read('--viz-tooltip-bg', f.tooltip.bg), border: read('--viz-tooltip-border', f.tooltip.border)},
    fontSans: read('--font-sans', f.fontSans),
    fontMono: read('--font-mono', f.fontMono),
    isDark: THEME_REGISTRY.find(t => t.id === themeId)?.category !== 'light',
    highContrast,
  }
}

export function useChartTheme(): ChartTheme {
  const theme = useUIStore(s => s.resolvedTheme)
  const highContrast = useSettingsStore(s => s.settings.appearance.highContrast)
  return useMemo(() => readChartTheme(theme, highContrast), [theme, highContrast])
}
