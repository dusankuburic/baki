import {describe, it, expect} from 'vitest'
import {computeDiff} from './PatchPreviewModal'

describe('computeDiff', () => {
  it('returns empty for identical texts', () => {
    expect(computeDiff('hello\nworld', 'hello\nworld')).toEqual([])
  })

  it('detects a single inserted line', () => {
    const diff = computeDiff('a\nb', 'a\nnew\nb')
    const added = diff.filter(d => d.type === 'added')
    expect(added).toHaveLength(1)
    expect(added[0].text).toBe('new')
  })

  it('detects a single removed line', () => {
    const diff = computeDiff('a\nold\nb', 'a\nb')
    const removed = diff.filter(d => d.type === 'removed')
    expect(removed).toHaveLength(1)
    expect(removed[0].text).toBe('old')
  })

  it('detects a replaced line (remove + add)', () => {
    const diff = computeDiff('a\nold\nb', 'a\nnew\nb')
    const removed = diff.filter(d => d.type === 'removed')
    const added = diff.filter(d => d.type === 'added')
    expect(removed).toHaveLength(1)
    expect(added).toHaveLength(1)
    expect(removed[0].text).toBe('old')
    expect(added[0].text).toBe('new')
  })

  it('trims context to ±3 lines around changes', () => {
    const original = Array.from({length: 20}, (_, i) => `line${i}`).join('\n')
    const patched = original.replace('line10', 'CHANGED')
    const diff = computeDiff(original, patched)
    // Should be much shorter than 20 lines (trimmed to ±3 context + ellipsis)
    expect(diff.length).toBeLessThanOrEqual(10)
  })

  it('adds ellipsis when trimming context', () => {
    const original = Array.from({length: 20}, (_, i) => `line${i}`).join('\n')
    const patched = original.replace('line10', 'CHANGED')
    const diff = computeDiff(original, patched)
    // Should have at least one ellipsis marker (context at start or end trimmed)
    const hasEllipsis = diff.some(d => d.text === '⋯')
    expect(hasEllipsis).toBe(true)
  })

  it('handles empty original', () => {
    const diff = computeDiff('', 'new content')
    // Empty string splits to [''], which is a removed line; the new content is added
    expect(diff.some(d => d.type === 'added' && d.text === 'new content')).toBe(true)
  })

  it('handles empty patched', () => {
    const diff = computeDiff('old content', '')
    expect(diff.some(d => d.type === 'removed' && d.text === 'old content')).toBe(true)
  })
})
