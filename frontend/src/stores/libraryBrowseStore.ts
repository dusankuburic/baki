import {create} from 'zustand'
import type {LibraryScope, LibrarySort} from '@/api/library'

// UI state for the Cloud Library workspace (browse view). Pure presentation —
// nothing persisted, nothing fetched here. Lives outside the workspace
// component so that toolbar/filter-rail/grid/detail can subscribe to slices
// without prop-drilling, and so re-opening the view restores the last
// selection within the same session.
export type LibraryViewMode = 'grid' | 'list'

interface LibraryBrowseState {
  view: LibraryViewMode
  scope: LibraryScope
  // null = include flows from every org the user belongs to (the default
  // "all orgs" view). Otherwise, only flows from one of these orgs (an empty
  // string represents the personal "no org" bucket).
  selectedOrgIds: ReadonlySet<string> | null
  sort: LibrarySort
  query: string
  page: number
  selectedFlowId: string | null
  pageSize: number

  setView: (v: LibraryViewMode) => void
  setScope: (s: LibraryScope) => void
  setSelectedOrgIds: (ids: ReadonlySet<string> | null) => void
  toggleOrg: (id: string) => void
  setSort: (s: LibrarySort) => void
  setQuery: (q: string) => void
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
  page: 0,
  selectedFlowId: null as string | null,
  pageSize: 24,
}

export const useLibraryBrowseStore = create<LibraryBrowseState>((set, get) => ({
  ...DEFAULTS,

  setView: (v) => set({view: v}),
  setScope: (s) => set({scope: s, page: 0, selectedFlowId: null}),
  setSelectedOrgIds: (ids) => set({selectedOrgIds: ids, page: 0, selectedFlowId: null}),
  toggleOrg: (id) => {
    const current = get().selectedOrgIds
    // Going from "all" (null) to a curated set: start from just-this-one.
    if (current === null) {
      set({selectedOrgIds: new Set([id]), page: 0, selectedFlowId: null})
      return
    }
    const next = new Set(current)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    // Empty set is a degenerate state (nothing visible). Snap back to "all".
    set({selectedOrgIds: next.size === 0 ? null : next, page: 0, selectedFlowId: null})
  },
  setSort: (s) => set({sort: s, page: 0}),
  setQuery: (q) => set({query: q, page: 0, selectedFlowId: null}),
  setPage: (p) => set({page: Math.max(0, p)}),
  setSelectedFlow: (id) => set({selectedFlowId: id}),
  reset: () => set(DEFAULTS),
}))
