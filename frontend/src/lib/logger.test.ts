import {describe, it, expect, vi, afterEach} from 'vitest'
import {logger} from './logger'

describe('logger.error', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('always forwards to console.error regardless of env', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})
    logger.error('boom', {detail: 1})
    expect(spy).toHaveBeenCalledWith('boom', {detail: 1})
  })
})

describe('logger.warn', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('forwards to console.warn in dev mode', () => {
    const spy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    logger.warn('careful')
    // import.meta.env.DEV is true under vitest by default.
    expect(spy).toHaveBeenCalledWith('careful')
  })
})
