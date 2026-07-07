import {describe, it, expect} from 'vitest'
import {categoryBadgeClass} from './findingsColors'

describe('categoryBadgeClass', () => {
  it('returns the matching color+background pair for a known category', () => {
    expect(categoryBadgeClass('Security')).toBe('bg-red-500/10 text-red-400')
  })

  it('falls back to a neutral class for an unknown category', () => {
    expect(categoryBadgeClass('Unknown')).toBe('bg-surface-3 text-text-tertiary')
  })

  it('falls back to a neutral class when category is undefined', () => {
    expect(categoryBadgeClass(undefined)).toBe('text-text-tertiary bg-surface-3')
  })
})
