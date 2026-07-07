import {describe, it, expect, vi} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useListNavigation} from './useListNavigation'

function key(k: string) {
  return {key: k, preventDefault: vi.fn()}
}

describe('useListNavigation — clamp mode (default)', () => {
  it('clamps ArrowDown at the last index', () => {
    const onSelect = vi.fn()
    const onClose = vi.fn()
    const {result} = renderHook(() => useListNavigation({count: 3, onSelect, onClose}))

    act(() => result.current.handleKeyDown(key('ArrowDown')))
    act(() => result.current.handleKeyDown(key('ArrowDown')))
    act(() => result.current.handleKeyDown(key('ArrowDown')))
    expect(result.current.activeIndex).toBe(2)
  })

  it('clamps ArrowUp at 0', () => {
    const {result} = renderHook(() => useListNavigation({count: 3, onSelect: vi.fn(), onClose: vi.fn()}))
    act(() => result.current.handleKeyDown(key('ArrowUp')))
    expect(result.current.activeIndex).toBe(0)
  })
})

describe('useListNavigation — wrap mode', () => {
  it('wraps ArrowDown from the last index back to 0', () => {
    const {result} = renderHook(() => useListNavigation({count: 3, onSelect: vi.fn(), onClose: vi.fn(), mode: 'wrap'}))
    act(() => result.current.setActiveIndex(2))
    act(() => result.current.handleKeyDown(key('ArrowDown')))
    expect(result.current.activeIndex).toBe(0)
  })

  it('wraps ArrowUp from 0 back to the last index', () => {
    const {result} = renderHook(() => useListNavigation({count: 3, onSelect: vi.fn(), onClose: vi.fn(), mode: 'wrap'}))
    act(() => result.current.handleKeyDown(key('ArrowUp')))
    expect(result.current.activeIndex).toBe(2)
  })
})

describe('useListNavigation — select/close', () => {
  it('calls onSelect with the active index on Enter', () => {
    const onSelect = vi.fn()
    const {result} = renderHook(() => useListNavigation({count: 3, onSelect, onClose: vi.fn()}))
    act(() => result.current.setActiveIndex(1))
    act(() => result.current.handleKeyDown(key('Enter')))
    expect(onSelect).toHaveBeenCalledWith(1)
  })

  it('does not call onSelect on Enter when count is 0', () => {
    const onSelect = vi.fn()
    const {result} = renderHook(() => useListNavigation({count: 0, onSelect, onClose: vi.fn()}))
    act(() => result.current.handleKeyDown(key('Enter')))
    expect(onSelect).not.toHaveBeenCalled()
  })

  it('honors extraSelectKeys (e.g. Tab) for selection', () => {
    const onSelect = vi.fn()
    const {result} = renderHook(() => useListNavigation({count: 3, onSelect, onClose: vi.fn(), extraSelectKeys: ['Tab']}))
    act(() => result.current.handleKeyDown(key('Tab')))
    expect(onSelect).toHaveBeenCalledWith(0)
  })

  it('calls onClose on Escape', () => {
    const onClose = vi.fn()
    const {result} = renderHook(() => useListNavigation({count: 3, onSelect: vi.fn(), onClose}))
    act(() => result.current.handleKeyDown(key('Escape')))
    expect(onClose).toHaveBeenCalled()
  })

  it('calls preventDefault for every handled key', () => {
    const {result} = renderHook(() => useListNavigation({count: 3, onSelect: vi.fn(), onClose: vi.fn()}))
    const e = key('ArrowDown')
    act(() => result.current.handleKeyDown(e))
    expect(e.preventDefault).toHaveBeenCalled()
  })

  it('ignores unrelated keys', () => {
    const onSelect = vi.fn()
    const onClose = vi.fn()
    const {result} = renderHook(() => useListNavigation({count: 3, onSelect, onClose}))
    act(() => result.current.handleKeyDown(key('a')))
    expect(result.current.activeIndex).toBe(0)
    expect(onSelect).not.toHaveBeenCalled()
    expect(onClose).not.toHaveBeenCalled()
  })
})
