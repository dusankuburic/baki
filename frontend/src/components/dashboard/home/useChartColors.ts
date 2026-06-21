import {useMemo} from 'react'
import {useUIStore} from '@/stores/uiStore'

// Recharts renders SVG and we pass colors as fill/stroke props. Rather than rely
// on `var(--token)` resolving inside SVG presentation attributes (inconsistent on
// the web), we resolve the design tokens to concrete hex once and re-resolve when
// the theme changes — see the dashboard plan §1.
export interface ChartColors {
  success: string
  warning: string
  error: string
  brand400: string
  brand500: string
  brand600: string
  surface3: string
  borderStrong: string
  textTertiary: string
}

function readVar(name: string, fallback: string): string {
  if (typeof window === 'undefined') return fallback
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return v || fallback
}

export function useChartColors(): ChartColors {
  const theme = useUIStore(s => s.resolvedTheme)
  return useMemo<ChartColors>(() => {
    // resolvedTheme swaps the :root variables, so it's the memo's invalidation
    // key — referenced here so it counts as a genuine dependency.
    void theme
    return {
      success: readVar('--success', '#22c55e'),
      warning: readVar('--warning', '#eab308'),
      error: readVar('--error', '#ef4444'),
      brand400: readVar('--brand-400', '#818cf8'),
      brand500: readVar('--brand-500', '#5b61ef'),
      brand600: readVar('--brand-600', '#4f46e5'),
      surface3: readVar('--surface-3', '#26262d'),
      borderStrong: readVar('--border-strong', '#3f3f47'),
      textTertiary: readVar('--text-tertiary', '#71717a'),
    }
  }, [theme])
}

// healthColor maps a 0–100 score to the semantic health palette.
export function healthColor(score: number, c: ChartColors): string {
  if (score >= 80) return c.success
  if (score >= 50) return c.warning
  return c.error
}
