import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {usePaneResize} from './usePaneResize'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'

vi.mock('@/api', () => ({
  settingsApi: {
    getSettings: vi.fn().mockResolvedValue(null),
    updateSettings: vi.fn().mockResolvedValue(undefined),
  },
}))

const initialState = useSettingsStore.getState()

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
    expect(useSettingsStore.getState().settings.layout.sidebarWidth).toBe(280)
    expect(result.current.sidebarWidth).toBe(280)
  })

  it('tells the graphs nothing: a React Flow graph does not repaint when its pane resizes', () => {
    const onGraphResize = vi.fn()
    window.addEventListener('graph:resize', onGraphResize)
    try {
      const {result} = renderHook(() => usePaneResize())
      const sidebar = makeDiv(280)
      const inspector = makeDiv(320)
      act(() => {
        result.current.sidebarRef.current = sidebar
        result.current.inspectorRef.current = inspector
      })

      act(() => result.current.handleSidebarDrag(10))
      act(() => result.current.handleInspectorDrag(-10))
      act(() => result.current.handleInspectorResizeEnd())

      expect(inspector.style.width).toBe('330px')
      expect(onGraphResize).not.toHaveBeenCalled()
    } finally {
      window.removeEventListener('graph:resize', onGraphResize)
    }
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

describe('usePaneResize — inspector cap follows the window', () => {
  const originalWidth = window.innerWidth
  function setViewport(width: number) {
    Object.defineProperty(window, 'innerWidth', {configurable: true, writable: true, value: width})
  }
  function mountInspector(width: number) {
    const {result} = renderHook(() => usePaneResize())
    const div = makeDiv(width)
    act(() => {
      result.current.inspectorRef.current = div
    })
    return {result, div}
  }
  afterEach(() => {
    setViewport(originalWidth)
    useUIStore.setState({sidebarCollapsed: false})
  })

  it('leaves the editor at least 480px on a 1600px window', () => {
    setViewport(1600)
    const {result, div} = mountInspector(320)
    act(() => result.current.handleInspectorDrag(-10000))
    expect(div.style.width).toBe('840px')
    expect(result.current.inspectorMax).toBe(840)
  })

  it('never grows past 1000px', () => {
    setViewport(3000)
    const {result, div} = mountInspector(320)
    act(() => result.current.handleInspectorDrag(-10000))
    expect(div.style.width).toBe('1000px')
  })

  it('counts a collapsed sidebar as zero width', () => {
    setViewport(1600)
    useUIStore.setState({sidebarCollapsed: true})
    const {result, div} = mountInspector(320)
    act(() => result.current.handleInspectorDrag(-10000))
    expect(div.style.width).toBe('1000px')
  })

  it('keeps the old 560px cap on a narrow window', () => {
    setViewport(1024)
    const {result, div} = mountInspector(320)
    act(() => result.current.handleInspectorDrag(-10000))
    expect(div.style.width).toBe('560px')
  })

  it('jumps to either limit and commits once', async () => {
    setViewport(1600)
    const {result, div} = mountInspector(320)
    await act(async () => {
      result.current.handleInspectorJump('max')
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(div.style.width).toBe('840px')
    expect(useSettingsStore.getState().settings.layout.inspectorWidth).toBe(840)
    await act(async () => {
      result.current.handleInspectorJump('min')
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(useSettingsStore.getState().settings.layout.inspectorWidth).toBe(280)
  })
})
