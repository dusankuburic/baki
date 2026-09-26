import {afterEach, describe, expect, it, vi} from 'vitest'
import {CHUNK_RELOAD_WINDOW_MS, isChunkLoadError, reloadForChunkError} from './chunkLoad'

describe('isChunkLoadError', () => {
  it.each([
    'Failed to fetch dynamically imported module: http://localhost:8080/assets/BlockView-DIpIx-nW.js',
    'error loading dynamically imported module: http://localhost:8080/assets/AITab-CWH9-ePf.js',
    'Importing a module script failed.',
    'Unable to preload CSS for /assets/index-BScJjtuO.css',
  ])('recognises "%s"', message => {
    expect(isChunkLoadError(new TypeError(message))).toBe(true)
  })

  it('ignores other errors and non-errors', () => {
    expect(isChunkLoadError(new TypeError("Cannot read properties of undefined (reading 'id')"))).toBe(false)
    expect(isChunkLoadError('Failed to fetch dynamically imported module')).toBe(false)
    expect(isChunkLoadError(null)).toBe(false)
  })
})

describe('reloadForChunkError', () => {
  afterEach(() => {
    sessionStorage.clear()
    vi.restoreAllMocks()
  })

  it('reloads at most once per window, and again once the window has passed', () => {
    const reload = vi.fn()
    const start = 1_000_000
    expect(reloadForChunkError(reload, start)).toBe(true)
    expect(reloadForChunkError(reload, start + CHUNK_RELOAD_WINDOW_MS - 1)).toBe(false)
    expect(reload).toHaveBeenCalledTimes(1)

    expect(reloadForChunkError(reload, start + CHUNK_RELOAD_WINDOW_MS)).toBe(true)
    expect(reload).toHaveBeenCalledTimes(2)
  })

  it('is not blocked by the one-shot flag earlier builds left in the session', () => {
    sessionStorage.setItem('baki.stale-chunk-reloaded', '1')
    const reload = vi.fn()
    expect(reloadForChunkError(reload)).toBe(true)
    expect(reload).toHaveBeenCalledOnce()
  })

  it('never reloads automatically when session storage is unavailable', () => {
    vi.spyOn(sessionStorage, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError')
    })
    const reload = vi.fn()
    expect(reloadForChunkError(reload)).toBe(false)
    expect(reload).not.toHaveBeenCalled()
  })
})
