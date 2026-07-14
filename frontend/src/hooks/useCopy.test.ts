import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useCopy} from './useCopy'

const writeClipboardMock = vi.fn()
vi.mock('@/lib/clipboard', () => ({
  writeClipboard: (text: string) => writeClipboardMock(text),
}))

beforeEach(() => {
  vi.useFakeTimers()
  writeClipboardMock.mockReset()
})

afterEach(() => {
  vi.useRealTimers()
})

// Flush the microtask queue (promise resolution) without advancing fake timers.
async function flushMicrotasks() {
  await act(async () => {
    await Promise.resolve()
    await Promise.resolve()
  })
}

describe('useCopy', () => {
  it('sets copied to true after a successful copy, then false after the timeout', async () => {
    writeClipboardMock.mockResolvedValue(undefined)
    const {result} = renderHook(() => useCopy(2000))

    act(() => result.current.copy('hello'))
    await flushMicrotasks()

    expect(writeClipboardMock).toHaveBeenCalledWith('hello')
    expect(result.current.copied).toBe(true)

    act(() => {
      vi.advanceTimersByTime(2000)
    })
    expect(result.current.copied).toBe(false)
  })

  it('does not throw when the clipboard write rejects', async () => {
    writeClipboardMock.mockRejectedValue(new Error('denied'))
    const {result} = renderHook(() => useCopy())

    expect(() => act(() => result.current.copy('x'))).not.toThrow()
    await flushMicrotasks()

    expect(writeClipboardMock).toHaveBeenCalled()
    expect(result.current.copied).toBe(false)
  })

  it('resets the timer on a second copy before the first timeout fires', async () => {
    writeClipboardMock.mockResolvedValue(undefined)
    const {result} = renderHook(() => useCopy(1000))

    act(() => result.current.copy('first'))
    await flushMicrotasks()
    expect(result.current.copied).toBe(true)

    act(() => {
      vi.advanceTimersByTime(600)
    })
    act(() => result.current.copy('second'))
    await flushMicrotasks()
    expect(writeClipboardMock).toHaveBeenCalledTimes(2)

    act(() => {
      vi.advanceTimersByTime(600)
    })
    // Original 1000ms timer (started at t=0) would've fired by t=1000 were it not reset at t=600.
    expect(result.current.copied).toBe(true)

    act(() => {
      vi.advanceTimersByTime(400)
    })
    expect(result.current.copied).toBe(false)
  })
})
