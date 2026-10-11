import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import type {LibraryScope, LibrarySort} from '@/api/library'

export type LibraryViewMode = 'grid' | 'list'

interface LibraryBrowseState {
  view: LibraryViewMode
  scope: LibraryScope
  selectedOrgIds: ReadonlySet<string> | null
  sort: LibrarySort
  query: string
  selectedTags: ReadonlySet<string>
  page: number
  selectedFlowId: string | null
  pageSize: number

  setView: (v: LibraryViewMode) => void
  setScope: (s: LibraryScope) => void
  setSelectedOrgIds: (ids: ReadonlySet<string> | null) => void
  toggleOrg: (id: string) => void
  setSort: (s: LibrarySort) => void
  setQuery: (q: string) => void
  toggleTag: (tag: string) => void
  clearTags: () => void
  setPage: (p: number) => void
  setSelectedFlow: (id: string | null) => void
  reset: () => void
}

const DEFAULTS = {
  view: 'grid' as LibraryViewMode,
  scope: 'all' as LibraryScope,
  selectedOrgIds: null,
  sort: 'updated_desc' as LibrarySort,
  query: '',
  selectedTags: new Set<string>() as ReadonlySet<string>,
  page: 0,
  selectedFlowId: null as string | null,
  pageSize: 24,
}

export const useLibraryBrowseStore = create<LibraryBrowseState>((set, get) => ({
  ...DEFAULTS,

  setView: v => set({view: v}),
  setScope: s => set({scope: s, page: 0, selectedFlowId: null}),
  setSelectedOrgIds: ids => set({selectedOrgIds: ids, page: 0, selectedFlowId: null}),
  toggleOrg: id => {
    const current = get().selectedOrgIds
    if (current === null) {
      set({selectedOrgIds: new Set([id]), page: 0, selectedFlowId: null})
      return
    }
    const next = new Set(current)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    set({selectedOrgIds: next.size === 0 ? null : next, page: 0, selectedFlowId: null})
  },
  setSort: s => set({sort: s, page: 0}),
  setQuery: q => set({query: q, page: 0, selectedFlowId: null}),
  toggleTag: tag => {
    const next = new Set(get().selectedTags)
    if (next.has(tag)) next.delete(tag)
    else next.add(tag)
    set({selectedTags: next, page: 0, selectedFlowId: null})
  },
  clearTags: () => set({selectedTags: new Set(), page: 0, selectedFlowId: null}),
  setPage: p => set({page: Math.max(0, p)}),
  setSelectedFlow: id => set({selectedFlowId: id}),
  reset: () => set(DEFAULTS),
}))

registerStoreReset(() => useLibraryBrowseStore.getState().reset())
