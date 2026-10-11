import {describe, it, expect, vi, beforeEach} from 'vitest'
import {registerStoreReset} from './storeRegistry'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 't'}),
  }),
}))

const errorSpy = vi.fn()
vi.mock('@/lib/logger', () => ({
  logger: {warn: vi.fn(), error: (...a: unknown[]) => errorSpy(...a)},
}))

let _local: Record<string, string> = {}
vi.stubGlobal('localStorage', {
  getItem: (k: string) => _local[k] ?? null,
  setItem: (k: string, v: string) => {
    _local[k] = v
  },
  removeItem: (k: string) => {
    delete _local[k]
  },
  get length() {
    return Object.keys(_local).length
  },
  key: (i: number) => Object.keys(_local)[i] ?? null,
})

import {useFlowStore} from './flowStore'
import {resetAllStores} from './authStore'
import {useSearchStore} from './searchStore'
import {useEditorStore} from './editorStore'
import {useUIStore} from './uiStore'
import {usePresenceStore} from './presenceStore'
import {useLibraryBrowseStore} from './libraryBrowseStore'
import {useAnalysisStore} from './analysisStore'
import {useSyncStore} from './syncStore'
import {syncManager} from '@/services/sync/SyncManager'

beforeEach(() => {
  _local = {}
})

describe('resetAllStores', () => {
  it('clears content-bearing stores and tears down the live session', async () => {
    useSearchStore.setState({results: [{} as never], totalCount: 1, query: 'secret'})
    useEditorStore.getState().openInGroup('subflow-A')
    useUIStore.setState({activeDiff: {foo: 'bar'} as never, mainPaneView: 'library', selectedVariable: 'v1'})
    useLibraryBrowseStore.setState({query: 'alice-secret', selectedFlowId: 'flow-1'})
    usePresenceStore.setState({
      users: {u2: {userId: 'u2', displayName: 'Bob'}},
      flowId: 'flow-1',
      status: 'connected',
    })
    useAnalysisStore.setState({
      reports: new Map([['flow-1', {flowId: 'flow-1', findings: []} as never]]),
      suppressedFindings: [{key: 'r1:b1', ruleId: 'r1', reason: 'x', suppressedAt: '2024'}],
      suppressedKeys: new Set(['r1:b1']),
      protectedFlowId: 'flow-1',
      isAnalyzing: true,
      variableLineage: {} as never,
    })

    await resetAllStores()

    expect(useSearchStore.getState().results).toHaveLength(0)
    expect(useEditorStore.getState().groups[0]!.tabs).toHaveLength(0)
    expect(useUIStore.getState().activeDiff).toBeNull()
    expect(useUIStore.getState().mainPaneView).toBe('home')
    expect(useUIStore.getState().selectedVariable).toBeNull()
    expect(useLibraryBrowseStore.getState().query).toBe('')
    expect(useLibraryBrowseStore.getState().selectedFlowId).toBeNull()

    expect(usePresenceStore.getState().flowId).toBeNull()
    expect(Object.keys(usePresenceStore.getState().users)).toHaveLength(0)
    expect(usePresenceStore.getState().status).toBe('disconnected')

    expect(useAnalysisStore.getState().reports.size).toBe(0)
    expect(useAnalysisStore.getState().suppressedFindings).toHaveLength(0)
    expect(useAnalysisStore.getState().suppressedKeys.size).toBe(0)
    expect(useAnalysisStore.getState().protectedFlowId).toBeNull()
    expect(useAnalysisStore.getState().isAnalyzing).toBe(false)
    expect(useAnalysisStore.getState().variableLineage).toBeNull()
  })

  it('preserves the resolved theme so the login screen does not flash', async () => {
    useUIStore.setState({resolvedTheme: 'nord'})
    await resetAllStores()
    expect(useUIStore.getState().resolvedTheme).toBe('nord')
  })

  it('discards the persisted offline sync queue so it cannot leak to the next user', async () => {
    localStorage.setItem('baki-sync-queue-flow-OLD', JSON.stringify({queue: [{}], counter: 1}))

    syncManager.start('flow-1')
    syncManager.enqueue({type: 'presence.update', payload: {}} as never)
    expect(localStorage.getItem('baki-sync-queue-flow-1')).not.toBeNull()

    await resetAllStores()

    expect(localStorage.getItem('baki-sync-queue-flow-1')).toBeNull()
    expect(localStorage.getItem('baki-sync-queue-flow-OLD')).toBeNull()
    expect(useSyncStore.getState().pendingCount).toBe(0)
  })

  it('clears previously-leaked fields (focusedFindingKey, visibleTypes, UI overlays, triage)', async () => {
    useAnalysisStore.setState({
      focusedFindingKey: 'r1:b1',
      selectedFindingIds: new Set(['r1:b1', 'r2:b2']),
      triageMap: new Map([['r1:b1', {status: 'resolved'} as never]]),
      baseline: {flowId: 'flow-1', version: 5} as never,
      savedViews: [{name: 'My View', filters: {}} as never],
    })
    useFlowStore.setState({
      visibleTypes: new Set(['ACTION'] as never),
      document: {id: 'leaked', name: 'Leaked'} as never,
    })
    useUIStore.setState({
      commandPaletteOpen: true,
      globalSearchOpen: true,
      settingsOpen: true,
    })

    await resetAllStores()

    expect(useAnalysisStore.getState().focusedFindingKey).toBeNull()
    expect(useAnalysisStore.getState().selectedFindingIds.size).toBe(0)
    expect(useAnalysisStore.getState().triageMap.size).toBe(0)
    expect(useAnalysisStore.getState().baseline).toBeNull()
    expect(useAnalysisStore.getState().savedViews).toHaveLength(1)
    expect(useFlowStore.getState().document).toBeNull()
    expect(useFlowStore.getState().visibleTypes.size).toBeGreaterThan(1)
    expect(useUIStore.getState().commandPaletteOpen).toBe(false)
    expect(useUIStore.getState().globalSearchOpen).toBe(false)
    expect(useUIStore.getState().settingsOpen).toBe(false)
  })

  it('isolates a failing handler and logs the error instead of swallowing it', async () => {
    const okBefore = errorSpy.mock.calls.length
    const throwing = vi.fn(() => {
      throw new Error('boom')
    })
    const after = vi.fn(() => {})
    registerStoreReset(throwing)
    registerStoreReset(after)

    await resetAllStores()

    expect(throwing).toHaveBeenCalled()
    expect(after).toHaveBeenCalled()
    expect(errorSpy.mock.calls.length).toBeGreaterThan(okBefore)
  })
})
