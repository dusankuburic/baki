import {describe, it, expect, afterEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {CHART_THEME, useChartTheme} from './useChartTheme'
import {useUIStore} from '@/stores/uiStore'
import {useSettingsStore} from '@/stores/settingsStore'

const settingsInitial = useSettingsStore.getState()

afterEach(() => {
  useUIStore.setState({resolvedTheme: 'dark'})
  useSettingsStore.setState(settingsInitial, true)
})

describe('CHART_THEME', () => {
  it('references the CSS tokens so the browser restyles charts with the page', () => {
    const t = CHART_THEME
    expect(t.series).toHaveLength(8)
    expect(t.seq).toHaveLength(4)
    expect(t.series[0]).toBe('var(--viz-1)')
    expect(t.series[7]).toBe('var(--viz-8)')
    expect(t.seq[0]).toBe('var(--viz-seq-1)')
    expect(t.seq[3]).toBe('var(--viz-seq-4)')
    expect(t.status).toEqual({
      success: 'var(--success)',
      warning: 'var(--warning)',
      error: 'var(--error)',
      info: 'var(--info)',
    })
    expect(t.emphasis).toBe('var(--viz-emphasis)')
    expect(t.surface).toBe('var(--surface-2)')
    const every = [
      ...t.series,
      ...t.seq,
      t.emphasis,
      t.muted,
      t.grid,
      t.axis,
      t.tick,
      t.crosshair,
      t.surface,
      t.tooltip.bg,
      t.tooltip.border,
      t.text.primary,
      t.text.secondary,
      t.text.tertiary,
      t.fontSans,
      t.fontMono,
    ]
    for (const c of every) expect(c).toMatch(/^var\(--[a-z0-9-]+\)$/)
  })
})

describe('useChartTheme', () => {
  it('hands out the same palette across theme and high-contrast changes (no re-render needed)', () => {
    const {result} = renderHook(() => useChartTheme())
    const first = result.current
    expect(first).toBe(CHART_THEME)

    act(() => useUIStore.setState({resolvedTheme: 'light'}))
    expect(result.current).toBe(first)

    act(() =>
      useSettingsStore.setState(s => ({
        settings: {...s.settings, appearance: {...s.settings.appearance, highContrast: true}},
      })),
    )
    expect(result.current).toBe(first)
  })
})
