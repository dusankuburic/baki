import {describe, it, expect} from 'vitest'
import {
  formatCompact,
  formatCount,
  formatCompactIntl,
  formatCurrency,
  formatPercent,
  parseDay,
  formatDateTick,
  formatDateLong,
  formatDelta,
} from './format'

describe('formatCompact', () => {
  it('renders numbers under 1000 as-is', () => {
    expect(formatCompact(0)).toBe('0')
    expect(formatCompact(999)).toBe('999')
  })

  it('abbreviates thousands with one decimal and a "k" suffix', () => {
    expect(formatCompact(1000)).toBe('1.0k')
    expect(formatCompact(5300)).toBe('5.3k')
    expect(formatCompact(999_999)).toBe('1000.0k')
  })

  it('abbreviates millions with one decimal and an "M" suffix', () => {
    expect(formatCompact(1_000_000)).toBe('1.0M')
    expect(formatCompact(2_450_000)).toBe('2.5M')
  })
})

describe('formatCount', () => {
  it('adds locale thousands separators', () => {
    expect(formatCount(1234)).toBe((1234).toLocaleString())
  })

  it('renders small numbers without separators', () => {
    expect(formatCount(5)).toBe('5')
  })
})

describe('Intl helpers (en-US pinned)', () => {
  it('formatCompactIntl abbreviates with at most one decimal', () => {
    expect(formatCompactIntl(0, 'en-US')).toBe('0')
    expect(formatCompactIntl(999, 'en-US')).toBe('999')
    expect(formatCompactIntl(1234, 'en-US')).toBe('1.2K')
    expect(formatCompactIntl(20_971, 'en-US')).toBe('21K')
    expect(formatCompactIntl(2_450_000, 'en-US')).toBe('2.5M')
  })

  it('formatCurrency keeps cents, and four decimals only for sub-cent amounts', () => {
    expect(formatCurrency(0.22, 'en-US')).toBe('$0.22')
    expect(formatCurrency(1234.5, 'en-US')).toBe('$1,234.50')
    expect(formatCurrency(0.0042, 'en-US')).toBe('$0.0042')
    expect(formatCurrency(0, 'en-US')).toBe('$0.00')
  })

  it('formatPercent rounds to the requested digits', () => {
    expect(formatPercent(0.98, 0, 'en-US')).toBe('98%')
    expect(formatPercent(0.3333, 1, 'en-US')).toBe('33.3%')
  })

  it('reads YYYY-MM-DD as a local day, not UTC midnight', () => {
    const d = parseDay('2026-09-24')!
    expect(d.getFullYear()).toBe(2026)
    expect(d.getMonth()).toBe(8)
    expect(d.getDate()).toBe(24)
    expect(parseDay('not a date')).toBeNull()
    expect(formatDateTick('2026-09-24', 'en-US')).toBe('Sep 24')
    expect(formatDateLong('2026-09-24', 'en-US')).toBe('Thu, Sep 24, 2026')
    expect(formatDateTick('garbage', 'en-US')).toBe('garbage')
  })

  it('formatDelta signs a percent change, or an absolute one from zero, or nothing', () => {
    expect(formatDelta(120, 100, 'en-US')).toEqual({text: '+20%', direction: 'up'})
    expect(formatDelta(80, 100, 'en-US')).toEqual({text: '-20%', direction: 'down'})
    expect(formatDelta(5, 0, 'en-US')).toEqual({text: '+5', direction: 'up'})
    expect(formatDelta(100, 100, 'en-US')).toEqual({text: '±0', direction: 'flat'})
    expect(formatDelta(100, null, 'en-US')).toEqual({text: '', direction: 'none'})
  })
})
