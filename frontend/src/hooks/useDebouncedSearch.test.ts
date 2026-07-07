import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useDebouncedSearch} from './useDebouncedSearch'

beforeEach(() => {
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('useDebouncedSearch', () => {
  it('calls onSearch with the trimmed query after the delay', () => {
    const onSearch = vi.fn()
    const {result} = renderHook(() => useDebouncedSearch({delay: 150, onSearch}))

    act(() => result.current.search('  hello  '))
    expect(onSearch).not.toHaveBeenCalled()

    act(() => { vi.advanceTimersByTime(150) })
    expect(onSearch).toHaveBeenCalledWith('hello')
  })

  it('calls onSearch immediately with an empty string for a blank query, bypassing the delay', () => {
    const onSearch = vi.fn()
    const {result} = renderHook(() => useDebouncedSearch({onSearch}))

    act(() => result.current.search('   '))
    expect(onSearch).toHaveBeenCalledWith('')
  })

  it('cancels a pending debounce when search is called again before the delay elapses', () => {
    const onSearch = vi.fn()
    const {result} = renderHook(() => useDebouncedSearch({delay: 150, onSearch}))

    act(() => result.current.search('first'))
    act(() => { vi.advanceTimersByTime(100) })
    act(() => result.current.search('second'))
    act(() => { vi.advanceTimersByTime(100) })
    expect(onSearch).not.toHaveBeenCalled()

    act(() => { vi.advanceTimersByTime(50) })
    expect(onSearch).toHaveBeenCalledTimes(1)
    expect(onSearch).toHaveBeenCalledWith('second')
  })

  it('uses the latest onSearch callback even if search was called before it updated', () => {
    const onSearchA = vi.fn()
    const onSearchB = vi.fn()
    const {result, rerender} = renderHook(
      ({onSearch}) => useDebouncedSearch({delay: 150, onSearch}),
      {initialProps: {onSearch: onSearchA}},
    )

    act(() => result.current.search('query'))
    rerender({onSearch: onSearchB})
    act(() => { vi.advanceTimersByTime(150) })

    expect(onSearchA).not.toHaveBeenCalled()
    expect(onSearchB).toHaveBeenCalledWith('query')
  })

  it('clears the pending timer on unmount', () => {
    const onSearch = vi.fn()
    const {result, unmount} = renderHook(() => useDebouncedSearch({delay: 150, onSearch}))
    act(() => result.current.search('x'))
    unmount()
    act(() => { vi.advanceTimersByTime(150) })
    expect(onSearch).not.toHaveBeenCalled()
  })
})
