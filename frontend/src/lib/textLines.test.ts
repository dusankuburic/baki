import {describe, it, expect} from 'vitest'
import {countLines, countNewlines, lineCountAfterEdit, visibleLineRange} from './textLines'

describe('countLines / countNewlines', () => {
  it('counts like a textarea: an empty buffer is one line', () => {
    expect(countLines('')).toBe(1)
    expect(countLines('a')).toBe(1)
    expect(countLines('a\n')).toBe(2)
    expect(countLines('a\nb\nc')).toBe(3)
  })

  it('counts only inside the given region', () => {
    const s = '\n\nab\ncd\n'
    expect(countNewlines(s)).toBe(4)
    expect(countNewlines(s, 2)).toBe(2)
    expect(countNewlines(s, 2, 5)).toBe(1)
    expect(countNewlines(s, 3, 3)).toBe(0)
  })
})

describe('lineCountAfterEdit', () => {
  const expectIncremental = (prev: string, next: string) => {
    expect(lineCountAfterEdit(prev, next, countLines(prev))).toBe(countLines(next))
  }

  it('tracks inserts, deletes and replacements anywhere in the buffer', () => {
    expectIncremental('a\nb\nc', 'a\nb\nX\nc')
    expectIncremental('a\nb\nc', 'a\nc')
    expectIncremental('a\nb\nc', '\n\na\nb\nc')
    expectIncremental('a\nb\nc', 'a\nb\nc\n\n')
    expectIncremental('a\nb\nc', 'a\nb\nc')
    expectIncremental('a\nb\nc', '')
    expectIncremental('', 'x\ny')
    expectIncremental('one\ntwo\nthree', 'uno\ndos\ntres\ncuatro')
    expectIncremental('abc', 'a\nb\nc')
    expectIncremental('a\nb\nc', 'abc')
  })

  it('does not double-count when the inserted text repeats its surroundings', () => {
    expectIncremental('a\na', 'a\na\na')
    expectIncremental('\n\n', '\n\n\n')
    expectIncremental('ab', 'aab')
    expectIncremental('aab', 'ab')
    expectIncremental('x\nx\nx', 'x\nx')
  })

  it('handles buffers larger than the comparison chunk (a 30k-line source)', () => {
    const lines = Array.from({length: 30_000}, (_, i) => `SET Var${i} TO ${i}`)
    const prev = lines.join('\n')
    expectIncremental(prev, prev.slice(0, 500_000) + '\nNEW LINE\n' + prev.slice(500_000))
    expectIncremental(prev, prev + '\n')
    expectIncremental(prev, '\n' + prev)
    expectIncremental(prev, prev.replace('SET Var20000', 'SET\nVar20000'))
    expectIncremental(prev, lines.slice(0, 10_000).join('\n'))
  })
})

describe('visibleLineRange', () => {
  it('covers the viewport plus the buffer and clamps to the document', () => {
    expect(visibleLineRange(0, 800, 20, 30_000, 20)).toEqual({first: 0, end: 60})
    expect(visibleLineRange(20 * 10_000, 800, 20, 30_000, 20)).toEqual({first: 9_980, end: 10_060})
    expect(visibleLineRange(20 * 29_990, 800, 20, 30_000, 20)).toEqual({first: 29_970, end: 30_000})
    expect(visibleLineRange(0, 800, 20, 3, 20)).toEqual({first: 0, end: 3})
  })

  it('never yields an inverted range', () => {
    expect(visibleLineRange(20 * 50_000, 800, 20, 100, 20)).toEqual({first: 49_980, end: 49_980})
  })
})
