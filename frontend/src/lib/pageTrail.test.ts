import {describe, it, expect} from 'vitest'
import {EMPTY_TRAIL, pastEnd, recordEnd, recordPage, trailCursor} from './pageTrail'

describe('pageTrail', () => {
  it('keeps the cursor each page answered for the page after it, under one key', () => {
    let trail = recordPage(EMPTY_TRAIL, 'k', 0, 'c1')
    trail = recordPage(trail, 'k', 1, 'c2')
    expect(trailCursor(trail, 'k', 1)).toBe('c1')
    expect(trailCursor(trail, 'k', 2)).toBe('c2')
    expect(trailCursor(trail, 'other', 1)).toBeUndefined()
    // A page answering none drops the stale cursor after it; a new key starts over.
    trail = recordPage(trail, 'k', 1, undefined)
    expect(trailCursor(trail, 'k', 2)).toBeUndefined()
    expect(trailCursor(trail, 'k', 1)).toBe('c1')
    trail = recordPage(trail, 'k2', 0, 'd1')
    expect(trailCursor(trail, 'k', 1)).toBeUndefined()
    expect(trailCursor(trail, 'k2', 1)).toBe('d1')
  })

  it('remembers that the server sends cursors, across keys, even one of no use', () => {
    expect(recordPage(EMPTY_TRAIL, 'k', 0, undefined).cursors).toBe(false)
    const trail = recordPage(EMPTY_TRAIL, 'k', 0, undefined, true)
    expect(trail.cursors).toBe(true)
    expect(trailCursor(trail, 'k', 1)).toBeUndefined()
    expect(recordPage(trail, 'k2', 0, undefined).cursors).toBe(true)
  })

  it('marks the first page found empty, until a cursor leads to or past it', () => {
    let trail = recordEnd(recordPage(EMPTY_TRAIL, 'k', 4, undefined), 'k', 5)
    expect(pastEnd(trail, 'k', 5)).toBe(true)
    expect(pastEnd(trail, 'k', 4)).toBe(false)
    expect(pastEnd(trail, 'other', 5)).toBe(false)
    trail = recordPage(trail, 'k', 4, undefined)
    expect(pastEnd(trail, 'k', 5)).toBe(true)
    trail = recordPage(trail, 'k', 4, 'c5')
    expect(pastEnd(trail, 'k', 5)).toBe(false)
  })
})
