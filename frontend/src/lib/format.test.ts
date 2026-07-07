import {describe, it, expect} from 'vitest'
import {formatCompact, formatCount} from './format'

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
