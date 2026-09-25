import {describe, it, expect, vi, afterEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {readChartTheme, useChartTheme} from './useChartTheme'
import {useUIStore} from '@/stores/uiStore'
import {useSettingsStore} from '@/stores/settingsStore'

function stubComputedStyle(vars: Record<string, string>) {
  return vi.spyOn(window, 'getComputedStyle').mockReturnValue({
    getPropertyValue: (name: string) => vars[name] ?? '',
  } as unknown as CSSStyleDeclaration)
}

afterEach(() => {
  vi.restoreAllMocks()
  useUIStore.setState({resolvedTheme: 'dark'})
})

describe('readChartTheme', () => {
  it('falls back to the Deep Dark palette when no token is computed (jsdom)', () => {
    stubComputedStyle({})
    const t = readChartTheme('dark', false)
    expect(t.series).toHaveLength(8)
    expect(t.seq).toHaveLength(4)
    for (const c of [...t.series, ...t.seq, t.emphasis, t.grid, t.surface]) expect(c).toMatch(/^#[0-9a-f]{6}$/i)
    expect(t.isDark).toBe(true)
    expect(t.highContrast).toBe(false)
  })

  it('reads the CSS tokens as they are computed on the root element', () => {
    stubComputedStyle({'--viz-1': '#123456', '--viz-seq-2': '#abcdef', '--success': '#00aa00', '--viz-grid': '#333333'})
    const t = readChartTheme('light', true)
    expect(t.series[0]).toBe('#123456')
    expect(t.series[1]).toMatch(/^#/) // fallback for the unset slot
    expect(t.seq[1]).toBe('#abcdef')
    expect(t.status.success).toBe('#00aa00')
    expect(t.grid).toBe('#333333')
    expect(t.isDark).toBe(false)
    expect(t.highContrast).toBe(true)
  })
})

describe('useChartTheme', () => {
  it('re-resolves when the theme or the high-contrast setting changes', () => {
    stubComputedStyle({})
    const {result} = renderHook(() => useChartTheme())
    const first = result.current
    expect(first.isDark).toBe(true)

    act(() => useUIStore.setState({resolvedTheme: 'light'}))
    expect(result.current).not.toBe(first)
    expect(result.current.isDark).toBe(false)

    const before = result.current
    act(() =>
      useSettingsStore.setState(s => ({
        settings: {...s.settings, appearance: {...s.settings.appearance, highContrast: true}},
      })),
    )
    expect(result.current).not.toBe(before)
    expect(result.current.highContrast).toBe(true)
  })
})
