import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useFullscreen} from './useFullscreen'

// jsdom has no Fullscreen API: install a minimal one whose request/exit calls
// flip `fullscreenElement` and fire `fullscreenchange`, like a browser does.
let fullscreenEl: Element | null = null
const request = vi.fn(() => {
  fullscreenEl = document.documentElement
  document.dispatchEvent(new Event('fullscreenchange'))
  return Promise.resolve()
})
const exit = vi.fn(() => {
  fullscreenEl = null
  document.dispatchEvent(new Event('fullscreenchange'))
  return Promise.resolve()
})

beforeEach(() => {
  fullscreenEl = null
  request.mockClear()
  exit.mockClear()
  Object.defineProperty(document, 'fullscreenElement', {configurable: true, get: () => fullscreenEl})
  Object.defineProperty(document.documentElement, 'requestFullscreen', {configurable: true, value: request})
  Object.defineProperty(document, 'exitFullscreen', {configurable: true, value: exit})
})

afterEach(() => {
  delete (document as {fullscreenElement?: unknown}).fullscreenElement
  delete (document.documentElement as {requestFullscreen?: unknown}).requestFullscreen
  delete (document as {exitFullscreen?: unknown}).exitFullscreen
})

describe('useFullscreen', () => {
  it('toggles in and back out — the second call leaves fullscreen instead of re-requesting it', () => {
    const {result} = renderHook(() => useFullscreen())
    expect(result.current.isFullscreen).toBe(false)

    act(() => result.current.toggleFullscreen())
    expect(request).toHaveBeenCalledTimes(1)
    expect(result.current.isFullscreen).toBe(true)

    act(() => result.current.toggleFullscreen())
    expect(exit).toHaveBeenCalledTimes(1)
    expect(request).toHaveBeenCalledTimes(1)
    expect(result.current.isFullscreen).toBe(false)
  })

  it('follows a fullscreen exit it did not trigger (Escape, browser controls)', () => {
    const {result} = renderHook(() => useFullscreen())
    act(() => result.current.toggleFullscreen())
    expect(result.current.isFullscreen).toBe(true)

    act(() => {
      fullscreenEl = null
      document.dispatchEvent(new Event('fullscreenchange'))
    })
    expect(result.current.isFullscreen).toBe(false)
  })

  it('stops listening on unmount', () => {
    const remove = vi.spyOn(document, 'removeEventListener')
    const {unmount} = renderHook(() => useFullscreen())
    unmount()
    expect(remove).toHaveBeenCalledWith('fullscreenchange', expect.any(Function))
    remove.mockRestore()
  })
})
