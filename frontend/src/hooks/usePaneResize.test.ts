import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {usePaneResize} from './usePaneResize'
import {useSettingsStore} from '@/stores/settingsStore'

vi.mock('@/api', () => ({
  settingsApi: {
    getSettings: vi.fn().mockResolvedValue(null),
    updateSettings: vi.fn().mockResolvedValue(undefined),
  },
}))

const initialState = useSettingsStore.getState()

beforeEach(() => {
  vi.useFakeTimers()
  useSettingsStore.setState(initialState, true)
})

afterEach(() => {
  vi.useRealTimers()
})

describe('usePaneResize — sidebar', () => {
  it('reports the live-drag width while dragging, clamped to [200, 480]', () => {
    const {result} = renderHook(() => usePaneResize())
    expect(result.current.sidebarWidth).toBe(280)

    act(() => result.current.handleSidebarDrag(50))
    expect(result.current.sidebarWidth).toBe(330)

    act(() => result.current.handleSidebarDrag(1000))
    expect(result.current.sidebarWidth).toBe(480)
  })

  it('clamps the sidebar to the minimum width', () => {
    const {result} = renderHook(() => usePaneResize())
    act(() => result.current.handleSidebarDrag(-1000))
    expect(result.current.sidebarWidth).toBe(200)
  })

  it('persists to the settings store on resize end and clears the live width', async () => {
    const {result} = renderHook(() => usePaneResize())
    act(() => result.current.handleSidebarDrag(50))
    await act(async () => {
      result.current.handleSidebarResizeEnd()
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(330)
  })

  it('resets the sidebar to its default width', async () => {
    void useSettingsStore.getState().updateLayout({sidebarWidth: 400})
    const {result} = renderHook(() => usePaneResize())
    await act(async () => {
      void result.current.handleSidebarReset()
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(280)
  })
})

describe('usePaneResize — inspector', () => {
  it('drags in the opposite direction from the sidebar (negative delta widens)', () => {
    const {result} = renderHook(() => usePaneResize())
    expect(result.current.inspectorWidth).toBe(320)

    act(() => result.current.handleInspectorDrag(-50))
    expect(result.current.inspectorWidth).toBe(370)
  })

  it('clamps the inspector to [280, 560]', () => {
    const {result} = renderHook(() => usePaneResize())
    act(() => result.current.handleInspectorDrag(1000))
    expect(result.current.inspectorWidth).toBe(280)
    act(() => result.current.handleInspectorDrag(-10000))
    expect(result.current.inspectorWidth).toBe(560)
  })
})
