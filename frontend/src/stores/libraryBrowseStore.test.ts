import {describe, it, expect, beforeEach} from 'vitest'
import {useLibraryBrowseStore} from './libraryBrowseStore'

const initialState = useLibraryBrowseStore.getState()

beforeEach(() => {
  useLibraryBrowseStore.setState(initialState, true)
})

describe('setScope / setSelectedOrgIds / setQuery', () => {
  it('resets page and selectedFlowId when scope changes', () => {
    useLibraryBrowseStore.setState({page: 3, selectedFlowId: 'f1'})
    useLibraryBrowseStore.getState().setScope('mine')
    const s = useLibraryBrowseStore.getState()
    expect(s.scope).toBe('mine')
    expect(s.page).toBe(0)
    expect(s.selectedFlowId).toBeNull()
  })

  it('resets page and selectedFlowId when org filter changes', () => {
    useLibraryBrowseStore.setState({page: 2, selectedFlowId: 'f1'})
    useLibraryBrowseStore.getState().setSelectedOrgIds(new Set(['org-1']))
    const s = useLibraryBrowseStore.getState()
    expect(s.selectedOrgIds).toEqual(new Set(['org-1']))
    expect(s.page).toBe(0)
    expect(s.selectedFlowId).toBeNull()
  })

  it('resets page and selectedFlowId when the query changes', () => {
    useLibraryBrowseStore.setState({page: 1, selectedFlowId: 'f1'})
    useLibraryBrowseStore.getState().setQuery('hello')
    const s = useLibraryBrowseStore.getState()
    expect(s.query).toBe('hello')
    expect(s.page).toBe(0)
    expect(s.selectedFlowId).toBeNull()
  })
})

describe('toggleOrg', () => {
  it('starts a curated set with just the toggled id when coming from "all" (null)', () => {
    useLibraryBrowseStore.getState().toggleOrg('org-1')
    expect(useLibraryBrowseStore.getState().selectedOrgIds).toEqual(new Set(['org-1']))
  })

  it('adds an id to an existing curated set', () => {
    useLibraryBrowseStore.setState({selectedOrgIds: new Set(['org-1'])})
    useLibraryBrowseStore.getState().toggleOrg('org-2')
    expect(useLibraryBrowseStore.getState().selectedOrgIds).toEqual(new Set(['org-1', 'org-2']))
  })

  it('removes an id already in the curated set', () => {
    useLibraryBrowseStore.setState({selectedOrgIds: new Set(['org-1', 'org-2'])})
    useLibraryBrowseStore.getState().toggleOrg('org-1')
    expect(useLibraryBrowseStore.getState().selectedOrgIds).toEqual(new Set(['org-2']))
  })

  it('snaps back to "all" (null) when removing the last id leaves an empty set', () => {
    useLibraryBrowseStore.setState({selectedOrgIds: new Set(['org-1'])})
    useLibraryBrowseStore.getState().toggleOrg('org-1')
    expect(useLibraryBrowseStore.getState().selectedOrgIds).toBeNull()
  })
})

describe('setPage', () => {
  it('clamps negative pages to 0', () => {
    useLibraryBrowseStore.getState().setPage(-5)
    expect(useLibraryBrowseStore.getState().page).toBe(0)
  })

  it('accepts a positive page', () => {
    useLibraryBrowseStore.getState().setPage(4)
    expect(useLibraryBrowseStore.getState().page).toBe(4)
  })
})

describe('reset', () => {
  it('restores all fields to defaults', () => {
    const s = useLibraryBrowseStore.getState()
    s.setView('list')
    s.setScope('mine')
    s.setQuery('x')
    s.setPage(2)
    s.setSelectedFlow('f1')
    s.reset()
    const after = useLibraryBrowseStore.getState()
    expect(after.view).toBe('grid')
    expect(after.scope).toBe('all')
    expect(after.query).toBe('')
    expect(after.page).toBe(0)
    expect(after.selectedFlowId).toBeNull()
  })
})
