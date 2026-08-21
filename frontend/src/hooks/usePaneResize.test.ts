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

// makeDiv builds a minimal fake pane element recording style writes.
function makeDiv(initialWidth: number) {
  return {
    style: {width: initialWidth + 'px'},
  } as HTMLDivElement
}

beforeEach(() => {
  vi.useFakeTimers()
  useSettingsStore.setState(initialState, true)
})

afterEach(() => {
  vi.useRealTimers()
})

describe('usePaneResize — DOM-mutation drag contract', () => {
  it('mutates the pane element style during drag with NO store update per move', () => {
    const {result} = renderHook(() => usePaneResize())
    const div = makeDiv(280)
    act(() => {
      result.current.sidebarRef.current = div
    })

    act(() => result.current.handleSidebarDrag(50))
    act(() => result.current.handleSidebarDrag(10))
    expect(div.style.width).toBe('340px')
    // The committed width is untouched until resize end — no re-render storm.
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(280)
    expect(result.current.sidebarWidth).toBe(280)
  })

  it('clamps the sidebar to [200, 480]', () => {
    const {result} = renderHook(() => usePaneResize())
    const div = makeDiv(280)
    act(() => {
      result.current.sidebarRef.current = div
    })
    act(() => result.current.handleSidebarDrag(1000))
    expect(div.style.width).toBe('480px')
    act(() => result.current.handleSidebarDrag(-10000))
    expect(div.style.width).toBe('200px')
  })

  it('commits exactly once on resize end', async () => {
    const {result} = renderHook(() => usePaneResize())
    const div = makeDiv(280)
    act(() => {
      result.current.sidebarRef.current = div
    })
    act(() => result.current.handleSidebarDrag(50))
    act(() => result.current.handleSidebarDrag(10))
    await act(async () => {
      result.current.handleSidebarResizeEnd()
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(340)
    // A resize end with no drag in flight is a no-op.
    await act(async () => {
      result.current.handleSidebarResizeEnd()
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(340)
  })

  it('inspector drags in the opposite direction (negative delta widens)', () => {
    const {result} = renderHook(() => usePaneResize())
    const div = makeDiv(320)
    act(() => {
      result.current.inspectorRef.current = div
    })
    act(() => result.current.handleInspectorDrag(-50))
    expect(div.style.width).toBe('370px')
    act(() => result.current.handleInspectorDrag(10000))
    expect(div.style.width).toBe('280px')
  })

  it('resets the sidebar to its default width', async () => {
    const {result} = renderHook(() => usePaneResize())
    await act(async () => {
      void result.current.handleSidebarReset()
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(280)
  })
})
