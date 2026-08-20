import {describe, it, expect, vi, beforeEach} from 'vitest'
import {useGovernanceStore} from './governanceStore'
import type {GovernanceAlert} from '@/api/governance'

// Stub the API layer so tests don't hit a real backend.
const mockList = vi.fn()
const mockUnreadCount = vi.fn()
const mockMarkRead = vi.fn()
const mockMarkAllRead = vi.fn()
const mockDismiss = vi.fn()
const mockClear = vi.fn()

vi.mock('@/api/governance', () => ({
  governanceApi: {
    list: (...a: unknown[]) => mockList(...a),
    unreadCount: (...a: unknown[]) => mockUnreadCount(...a),
    markRead: (...a: unknown[]) => mockMarkRead(...a),
    markAllRead: (...a: unknown[]) => mockMarkAllRead(...a),
    dismiss: (...a: unknown[]) => mockDismiss(...a),
    clear: (...a: unknown[]) => mockClear(...a),
  },
}))

const baseAlert = (over: Partial<GovernanceAlert> = {}): GovernanceAlert => ({
  id: 'a1',
  flowId: 'f1',
  flowName: 'Flow One',
  type: 'drift',
  title: 'New findings in Flow One',
  severity: 'error',
  newErrors: 2,
  createdAt: new Date().toISOString(),
  ...over,
})

beforeEach(() => {
  vi.clearAllMocks()
  mockUnreadCount.mockResolvedValue({count: 0})
  mockList.mockResolvedValue([])
  mockMarkRead.mockResolvedValue({status: 'ok'})
  mockMarkAllRead.mockResolvedValue({status: 'ok'})
  mockDismiss.mockResolvedValue({status: 'ok'})
  mockClear.mockResolvedValue({status: 'ok'})
  // Partial reset (merge) — a full `setState(..., true)` would wipe the action
  // methods, which are part of the Zustand state object.
  useGovernanceStore.setState({
    alerts: [],
    unreadCount: 0,
    loading: false,
    panelOpen: false,
    lastError: null,
  })
})

describe('governanceStore', () => {
  it('refreshUnread updates the badge count', async () => {
    mockUnreadCount.mockResolvedValue({count: 3})
    await useGovernanceStore.getState().refreshUnread()
    expect(useGovernanceStore.getState().unreadCount).toBe(3)
  })

  it('refreshUnread swallows errors (best-effort poll)', async () => {
    mockUnreadCount.mockRejectedValue(new Error('boom'))
    await useGovernanceStore.getState().refreshUnread()
    // No throw; count stays at its previous value.
    expect(useGovernanceStore.getState().unreadCount).toBe(0)
  })

  it('openPanel loads the list and clears the badge (mark-all-read)', async () => {
    mockList.mockResolvedValue([baseAlert({id: 'a1'}), baseAlert({id: 'a2'})])
    await useGovernanceStore.getState().openPanel()
    const st = useGovernanceStore.getState()
    expect(st.panelOpen).toBe(true)
    expect(st.loading).toBe(false)
    expect(st.alerts).toHaveLength(2)
    expect(st.unreadCount).toBe(0)
    expect(mockMarkAllRead).toHaveBeenCalled()
  })

  it('markRead optimistically stamps a readAt and calls the API', async () => {
    useGovernanceStore.setState({alerts: [baseAlert({id: 'a1', readAt: undefined})]})
    await useGovernanceStore.getState().markRead('a1')
    const a = useGovernanceStore.getState().alerts[0]
    expect(a.readAt).toBeTruthy()
    expect(mockMarkRead).toHaveBeenCalledWith('a1')
  })

  it('markRead rolls back on API error', async () => {
    useGovernanceStore.setState({alerts: [baseAlert({id: 'a1', readAt: undefined})]})
    mockMarkRead.mockRejectedValue(new Error('fail'))
    await useGovernanceStore.getState().markRead('a1')
    expect(useGovernanceStore.getState().alerts[0].readAt).toBeUndefined()
  })

  it('markAllRead rolls back alerts and badge on API error', async () => {
    useGovernanceStore.setState({
      alerts: [baseAlert({id: 'a1', readAt: undefined}), baseAlert({id: 'a2', readAt: undefined})],
      unreadCount: 2,
    })
    mockMarkAllRead.mockRejectedValue(new Error('fail'))
    await useGovernanceStore.getState().markAllRead()
    const st = useGovernanceStore.getState()
    // The optimistic "all read" must be undone — otherwise the badge claims 0
    // unread while the server still holds them unread.
    expect(st.unreadCount).toBe(2)
    expect(st.alerts.every(a => a.readAt === undefined)).toBe(true)
    expect(st.lastError).toBeTruthy()
  })

  it('dismiss hides the alert (stamps dismissedAt) and calls the API', async () => {
    useGovernanceStore.setState({alerts: [baseAlert({id: 'a1'})]})
    await useGovernanceStore.getState().dismiss('a1')
    expect(useGovernanceStore.getState().alerts[0].dismissedAt).toBeTruthy()
    expect(mockDismiss).toHaveBeenCalledWith('a1')
  })

  it('reloadList refreshes the list without re-acknowledging', async () => {
    mockList.mockResolvedValue([baseAlert({id: 'a1'}), baseAlert({id: 'a2'})])
    useGovernanceStore.setState({panelOpen: true, unreadCount: 2})
    await useGovernanceStore.getState().reloadList()
    const st = useGovernanceStore.getState()
    expect(st.alerts).toHaveLength(2)
    // reloadList must NOT call markAllRead (the panel was already acked on open;
    // a newly-arrived alert should stay unread until the user sees it).
    expect(mockMarkAllRead).not.toHaveBeenCalled()
    // unreadCount is untouched by reloadList (it doesn't know the new total;
    // refreshUnread owns the badge).
    expect(st.unreadCount).toBe(2)
  })

  it('reset clears state', () => {
    useGovernanceStore.setState({alerts: [baseAlert()], unreadCount: 5, panelOpen: true})
    useGovernanceStore.getState().reset()
    const st = useGovernanceStore.getState()
    expect(st.alerts).toEqual([])
    expect(st.unreadCount).toBe(0)
    expect(st.panelOpen).toBe(false)
  })
})
