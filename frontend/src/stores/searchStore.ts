import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import type {SearchResult} from '@/types'

interface SearchState {
  query: string
  results: SearchResult[]
  activeResultIndex: number
  isSearching: boolean
  totalCount: number
  focusRequest: number

  setQuery: (q: string) => void
  setResults: (results: SearchResult[], total: number) => void
  nextResult: () => void
  prevResult: () => void
  clear: () => void
  requestFocus: () => void
}

export const useSearchStore = create<SearchState>((set, _get) => ({
  query: '',
  results: [],
  activeResultIndex: -1,
  isSearching: false,
  totalCount: 0,
  focusRequest: 0,

  setQuery: (q) => set({query: q, isSearching: q.length > 0}),

  setResults: (results, total) => set({
    results,
    totalCount: total,
    activeResultIndex: results.length > 0 ? 0 : -1,
    isSearching: false,
  }),

  nextResult: () => set(state => {
    if (state.results.length === 0) return state
    return {activeResultIndex: (state.activeResultIndex + 1) % state.results.length}
  }),

  prevResult: () => set(state => {
    if (state.results.length === 0) return state
    return {activeResultIndex: (state.activeResultIndex - 1 + state.results.length) % state.results.length}
  }),

  clear: () => set({query: '', results: [], activeResultIndex: -1, isSearching: false, totalCount: 0}),

  requestFocus: () => set(state => ({focusRequest: state.focusRequest + 1, query: ''})),
}))

// Reset on logout (see storeRegistry).
registerStoreReset(() => useSearchStore.getState().clear())
