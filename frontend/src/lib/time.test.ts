import {describe, it, expect, vi, afterEach} from 'vitest'
import {relativeTime, absoluteTime} from './time'

const NOW = new Date('2026-01-15T12:00:00.000Z')

describe('relativeTime', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  it('returns "now" for timestamps under a minute old', () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    expect(relativeTime(new Date(NOW.getTime() - 30_000).toISOString())).toBe('now')
  })

  it('renders minutes for timestamps under an hour old', () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    expect(relativeTime(new Date(NOW.getTime() - 5 * 60_000).toISOString())).toBe('5m')
  })

  it('renders hours for timestamps under a day old', () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    expect(relativeTime(new Date(NOW.getTime() - 3 * 3_600_000).toISOString())).toBe('3h')
  })

  it('renders days for timestamps under a week old', () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    expect(relativeTime(new Date(NOW.getTime() - 2 * 86_400_000).toISOString())).toBe('2d')
  })

  it('renders weeks for timestamps a week or older', () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    expect(relativeTime(new Date(NOW.getTime() - 14 * 86_400_000).toISOString())).toBe('2w')
  })

  it('returns empty string for an invalid ISO string', () => {
    expect(relativeTime('not-a-date')).toBe('')
  })

  it('clamps future timestamps to "now" instead of going negative', () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    expect(relativeTime(new Date(NOW.getTime() + 60_000).toISOString())).toBe('now')
  })
})

describe('absoluteTime', () => {
  it('renders a locale date/time string for a valid ISO string', () => {
    expect(absoluteTime('2026-01-15T12:00:00.000Z')).toBe(new Date('2026-01-15T12:00:00.000Z').toLocaleString())
  })

  it('returns empty string for an invalid ISO string', () => {
    expect(absoluteTime('garbage')).toBe('')
  })
})
