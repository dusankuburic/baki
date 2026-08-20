import {describe, it, expect} from 'vitest'
import {toggleSetMember} from './collections'

describe('toggleSetMember', () => {
  it('adds a value not already in the set', () => {
    const result = toggleSetMember(new Set([1, 2]), 3)
    expect([...result]).toEqual([1, 2, 3])
  })

  it('removes a value already in the set', () => {
    const result = toggleSetMember(new Set([1, 2, 3]), 2)
    expect([...result]).toEqual([1, 3])
  })

  it('does not mutate the original set', () => {
    const original = new Set([1])
    toggleSetMember(original, 2)
    expect([...original]).toEqual([1])
  })

  it('returns a new Set instance', () => {
    const original = new Set([1])
    expect(toggleSetMember(original, 1)).not.toBe(original)
  })
})



