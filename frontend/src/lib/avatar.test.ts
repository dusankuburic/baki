import {describe, it, expect} from 'vitest'
import {userInitials, userColor} from './avatar'

describe('userInitials', () => {
  it('takes first+last initial for a multi-word name', () => {
    expect(userInitials('Jane Doe')).toBe('JD')
  })

  it('uses the first two letters for a single-word name', () => {
    expect(userInitials('Madonna')).toBe('MA')
  })

  it('collapses extra whitespace between name parts', () => {
    expect(userInitials('  Jane   Middle   Doe  ')).toBe('JD')
  })

  it('uppercases lowercase input', () => {
    expect(userInitials('jane doe')).toBe('JD')
  })
})

describe('userColor', () => {
  it('is deterministic for the same id', () => {
    expect(userColor('user-123')).toBe(userColor('user-123'))
  })

  it('returns a value from the fixed palette', () => {
    const palette = ['#5b61ef', '#8b5cf6', '#ec4899', '#ef4444', '#f97316', '#eab308', '#22c55e', '#06b6d4']
    expect(palette).toContain(userColor('any-user'))
  })

  it('handles the empty string without throwing', () => {
    expect(() => userColor('')).not.toThrow()
  })
})
