import {describe, it, expect} from 'vitest'
import {toggleSetMember, mapSet, mapDelete, mapUpdate} from './collections'

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

describe('mapSet', () => {
  it('sets a key without mutating the original map', () => {
    const original = new Map([['a', 1]])
    const result = mapSet(original, 'b', 2)
    expect([...result.entries()]).toEqual([['a', 1], ['b', 2]])
    expect(original.has('b')).toBe(false)
  })

  it('overwrites an existing key', () => {
    const result = mapSet(new Map([['a', 1]]), 'a', 99)
    expect(result.get('a')).toBe(99)
  })
})

describe('mapDelete', () => {
  it('removes a key without mutating the original map', () => {
    const original = new Map([['a', 1], ['b', 2]])
    const result = mapDelete(original, 'a')
    expect(result.has('a')).toBe(false)
    expect(original.has('a')).toBe(true)
  })

  it('is a no-op (returns equivalent map) when the key is absent', () => {
    const result = mapDelete(new Map([['a', 1]]), 'z')
    expect([...result.entries()]).toEqual([['a', 1]])
  })
})

describe('mapUpdate', () => {
  it('passes undefined to the updater when the key is absent', () => {
    const result = mapUpdate(new Map<string, number>(), 'a', prev => (prev ?? 0) + 1)
    expect(result.get('a')).toBe(1)
  })

  it('passes the existing value to the updater when the key is present', () => {
    const result = mapUpdate(new Map([['a', 5]]), 'a', prev => (prev ?? 0) + 1)
    expect(result.get('a')).toBe(6)
  })

  it('does not mutate the original map', () => {
    const original = new Map([['a', 1]])
    mapUpdate(original, 'a', prev => (prev ?? 0) + 1)
    expect(original.get('a')).toBe(1)
  })
})
