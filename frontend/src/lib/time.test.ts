import {describe, it, expect, vi, afterEach} from 'vitest'
import {relativeTime, absoluteTime, relativeTimeText} from './time'

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

describe('relativeTimeText', () => {
  const now = Date.parse('2026-09-24T12:00:00Z')
  const at = (offsetSecs: number) => new Date(now + offsetSecs * 1000).toISOString()

  it('reads as a sentence in either direction', () => {
    expect(relativeTimeText(at(-2 * 3600), now, 'en')).toBe('2 hours ago')
    expect(relativeTimeText(at(-5 * 60), now, 'en')).toBe('5 minutes ago')
    expect(relativeTimeText(at(-3 * 86400), now, 'en')).toBe('3 days ago')
    expect(relativeTimeText(at(22 * 3600), now, 'en')).toBe('in 22 hours')
    expect(relativeTimeText(at(2 * 7 * 86400), now, 'en')).toBe('in 2 weeks')
  })

  it('uses the natural words for the nearest steps and the present', () => {
    expect(relativeTimeText(at(-86400), now, 'en')).toBe('yesterday')
    expect(relativeTimeText(at(86400), now, 'en')).toBe('tomorrow')
    expect(relativeTimeText(at(-20), now, 'en')).toBe('now')
  })

  it('is empty for an unparseable timestamp', () => {
    expect(relativeTimeText('', now, 'en')).toBe('')
    expect(relativeTimeText('not a date', now, 'en')).toBe('')
  })
})
