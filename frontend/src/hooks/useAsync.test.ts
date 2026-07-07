import {describe, it, expect, vi} from 'vitest'
import {renderHook, waitFor, act} from '@testing-library/react'
import {useAsync} from './useAsync'

describe('useAsync', () => {
  it('starts loading, then resolves with data', async () => {
    const fn = vi.fn().mockResolvedValue('result')
    const {result} = renderHook(() => useAsync(fn, []))

    expect(result.current.isLoading).toBe(true)
    await waitFor(() => expect(result.current.isLoading).toBe(false))

    expect(result.current.data).toBe('result')
    expect(result.current.error).toBeNull()
  })

  it('captures an Error message on rejection', async () => {
    const fn = vi.fn().mockRejectedValue(new Error('boom'))
    const {result} = renderHook(() => useAsync(fn, []))

    await waitFor(() => expect(result.current.isLoading).toBe(false))
    expect(result.current.error).toBe('boom')
    expect(result.current.data).toBeNull()
  })

  it('stringifies a non-Error rejection', async () => {
    const fn = vi.fn().mockRejectedValue('plain string failure')
    const {result} = renderHook(() => useAsync(fn, []))

    await waitFor(() => expect(result.current.isLoading).toBe(false))
    expect(result.current.error).toBe('plain string failure')
  })

  it('re-runs fn when a dep changes', async () => {
    const fn = vi.fn().mockResolvedValue('v')
    const {rerender} = renderHook(({dep}) => useAsync(fn, [dep]), {initialProps: {dep: 1}})
    await waitFor(() => expect(fn).toHaveBeenCalledTimes(1))

    rerender({dep: 2})
    await waitFor(() => expect(fn).toHaveBeenCalledTimes(2))
  })

  it('refetch triggers another call without a dep change', async () => {
    const fn = vi.fn().mockResolvedValue('v')
    const {result} = renderHook(() => useAsync(fn, []))
    await waitFor(() => expect(fn).toHaveBeenCalledTimes(1))

    act(() => result.current.refetch())
    await waitFor(() => expect(fn).toHaveBeenCalledTimes(2))
  })

  it('setData lets the caller override the resolved data', async () => {
    const fn = vi.fn().mockResolvedValue('original')
    const {result} = renderHook(() => useAsync(fn, []))
    await waitFor(() => expect(result.current.data).toBe('original'))

    act(() => result.current.setData('overridden'))
    expect(result.current.data).toBe('overridden')
  })

  it('ignores a stale resolution after the component unmounts', async () => {
    let resolveFn: (v: string) => void = () => {}
    const fn = vi.fn(() => new Promise<string>(res => { resolveFn = res }))
    const {result, unmount} = renderHook(() => useAsync(fn, []))
    unmount()
    resolveFn('too late')
    await new Promise(r => setTimeout(r, 0))
    expect(result.current.data).toBeNull()
  })
})
