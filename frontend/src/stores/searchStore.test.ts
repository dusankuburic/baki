import {describe, it, expect, beforeEach} from 'vitest'
import {useSearchStore} from './searchStore'
import type {SearchResult} from '@/types'

const initialState = useSearchStore.getState()

beforeEach(() => {
  useSearchStore.setState(initialState, true)
})

function result(blockId: string): SearchResult {
  return {
    blockId, subflowId: 'sf1', matchedField: 'name', matchedText: 'x',
    score: 1, highlights: [],
  }
}

describe('setQuery', () => {
  it('marks isSearching true for a non-empty query', () => {
    useSearchStore.getState().setQuery('foo')
    expect(useSearchStore.getState().isSearching).toBe(true)
  })

  it('marks isSearching false for an empty query', () => {
    useSearchStore.getState().setQuery('')
    expect(useSearchStore.getState().isSearching).toBe(false)
  })
})

describe('setResults', () => {
  it('selects the first result as active when results are non-empty', () => {
    useSearchStore.getState().setResults([result('a'), result('b')], 2)
    const s = useSearchStore.getState()
    expect(s.activeResultIndex).toBe(0)
    expect(s.totalCount).toBe(2)
    expect(s.isSearching).toBe(false)
  })

  it('sets activeResultIndex to -1 when results are empty', () => {
    useSearchStore.getState().setResults([], 0)
    expect(useSearchStore.getState().activeResultIndex).toBe(-1)
  })
})

describe('nextResult / prevResult', () => {
  beforeEach(() => {
    useSearchStore.getState().setResults([result('a'), result('b'), result('c')], 3)
  })

  it('advances the active index and wraps at the end', () => {
    const {nextResult} = useSearchStore.getState()
    nextResult()
    expect(useSearchStore.getState().activeResultIndex).toBe(1)
    nextResult()
    expect(useSearchStore.getState().activeResultIndex).toBe(2)
    nextResult()
    expect(useSearchStore.getState().activeResultIndex).toBe(0)
  })

  it('retreats the active index and wraps at the start', () => {
    const {prevResult} = useSearchStore.getState()
    prevResult()
    expect(useSearchStore.getState().activeResultIndex).toBe(2)
  })

  it('is a no-op when there are no results', () => {
    useSearchStore.getState().clear()
    useSearchStore.getState().nextResult()
    expect(useSearchStore.getState().activeResultIndex).toBe(-1)
  })
})

describe('clear', () => {
  it('resets query, results, index, isSearching, and totalCount', () => {
    useSearchStore.setState({query: 'x', results: [result('a')], activeResultIndex: 0, isSearching: true, totalCount: 1})
    useSearchStore.getState().clear()
    const s = useSearchStore.getState()
    expect(s.query).toBe('')
    expect(s.results).toEqual([])
    expect(s.activeResultIndex).toBe(-1)
    expect(s.isSearching).toBe(false)
    expect(s.totalCount).toBe(0)
  })
})

describe('requestFocus', () => {
  it('increments focusRequest and clears the query', () => {
    useSearchStore.setState({query: 'stale'})
    useSearchStore.getState().requestFocus()
    const s = useSearchStore.getState()
    expect(s.focusRequest).toBe(1)
    expect(s.query).toBe('')
    useSearchStore.getState().requestFocus()
    expect(useSearchStore.getState().focusRequest).toBe(2)
  })
})
