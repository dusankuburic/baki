import {describe, it, expect, vi, beforeEach} from 'vitest'
import {registerStoreReset} from './storeRegistry'

// api/client (imported transitively by authStore) pulls in the platform adapter;
// stub it so importing the stores doesn't try to invoke Tauri.
vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 't'}),
  }),
}))

// Capture logger.error so the "reset handler failed" log is observable in tests.
const errorSpy = vi.fn()
vi.mock('@/lib/logger', () => ({
  logger: {warn: vi.fn(), error: (...a: unknown[]) => errorSpy(...a)},
}))

// jsdom's storage is incomplete in vitest — stub it (mirrors authStore.test.ts).
let _local: Record<string, string> = {}
vi.stubGlobal('localStorage', {
  getItem: (k: string) => _local[k] ?? null,
  setItem: (k: string, v: string) => {
    _local[k] = v
  },
  removeItem: (k: string) => {
    delete _local[k]
  },
  // length + key(i) so SyncManager.clearAllStorage's prefix scan works.
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
    // Seed prior-session state across stores that logout previously left intact.
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
      findingsByBlock: new Map([['flow-1', new Map()]]),
      suppressedFindings: [{key: 'r1:b1', ruleId: 'r1', reason: 'x', suppressedAt: '2024'}],
      suppressedKeys: new Set(['r1:b1']),
      protectedFlowId: 'flow-1',
      isAnalyzing: true,
      variableLineage: {} as never,
    })

    await resetAllStores()

    expect(useSearchStore.getState().results).toHaveLength(0)
    expect(useEditorStore.getState().groups[0].tabs).toHaveLength(0)
    expect(useUIStore.getState().activeDiff).toBeNull()
    expect(useUIStore.getState().mainPaneView).toBe('home')
    expect(useUIStore.getState().selectedVariable).toBeNull()
    expect(useLibraryBrowseStore.getState().query).toBe('')
    expect(useLibraryBrowseStore.getState().selectedFlowId).toBeNull()

    // presenceStore.disconnect() ran — proxy for the collaboration WebSocket +
    // offline sync queue being torn down.
    expect(usePresenceStore.getState().flowId).toBeNull()
    expect(Object.keys(usePresenceStore.getState().users)).toHaveLength(0)
    expect(usePresenceStore.getState().status).toBe('disconnected')

    expect(useAnalysisStore.getState().reports.size).toBe(0)
    expect(useAnalysisStore.getState().findingsByBlock.size).toBe(0)
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
    // An orphaned queue from a flow the user visited earlier this session, then
    // navigated away from (start() clears memory but not the old flow's storage).
    localStorage.setItem('baki-sync-queue-flow-OLD', JSON.stringify({queue: [{}], counter: 1}))

    // Queue an offline op on the currently-open flow; SyncManager persists it to
    // localStorage keyed by flow id (no user component), so without an explicit
    // discard the next user who reopens either flow would inherit the ops.
    syncManager.start('flow-1')
    syncManager.enqueue({type: 'presence.update', payload: {}} as never)
    expect(localStorage.getItem('baki-sync-queue-flow-1')).not.toBeNull()

    await resetAllStores()

    // Both the active flow's queue AND the orphaned earlier-flow queue are gone.
    expect(localStorage.getItem('baki-sync-queue-flow-1')).toBeNull()
    expect(localStorage.getItem('baki-sync-queue-flow-OLD')).toBeNull()
    expect(useSyncStore.getState().pendingCount).toBe(0)
  })

  // Regression guard: the following fields were previously NOT cleared on
  // logout, leaking state to the next user. Each was fixed individually; this
  // test pins them so the same class of bug can't recur silently.
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
    // Saved views are DEVICE-LOCAL prefs (persisted to localStorage) and
    // deliberately survive the session reset — blanking them in memory while
    // they live on disk made them vanish until reload.
    expect(useAnalysisStore.getState().savedViews).toHaveLength(1)
    expect(useFlowStore.getState().document).toBeNull()
    // visibleTypes resets to ALL_TYPES (non-empty, full set)
    expect(useFlowStore.getState().visibleTypes.size).toBeGreaterThan(1)
    expect(useUIStore.getState().commandPaletteOpen).toBe(false)
    expect(useUIStore.getState().globalSearchOpen).toBe(false)
    expect(useUIStore.getState().settingsOpen).toBe(false)
  })

  // L1-fe: a throwing handler must NOT abort the others, and its failure must
  // be logged (previously silently swallowed — a logout-time error masking
  // data-loss-adjacent bugs was invisible).
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
    expect(after).toHaveBeenCalled() // isolated — not aborted by the throw
    expect(errorSpy.mock.calls.length).toBeGreaterThan(okBefore) // logged, not swallowed
  })
})
