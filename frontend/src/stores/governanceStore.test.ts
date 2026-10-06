import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {useGovernanceStore, startGovernancePolling, stopGovernancePolling} from './governanceStore'
import type {GovernanceAlert} from '@/api/governance'

// Stub the API layer so tests don't hit a real backend.
const mockList = vi.fn()
const mockUnreadCount = vi.fn()
const mockMarkAllRead = vi.fn()
const mockDismiss = vi.fn()
const mockClear = vi.fn()

vi.mock('@/api/governance', () => ({
  governanceApi: {
    list: (...a: unknown[]) => mockList(...a),
    unreadCount: (...a: unknown[]) => mockUnreadCount(...a),
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
    showDismissed: false,
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

  it('dismiss drops the alert from the default list and calls the API', async () => {
    useGovernanceStore.setState({alerts: [baseAlert({id: 'a1'}), baseAlert({id: 'a2'})]})
    await useGovernanceStore.getState().dismiss('a1')
    expect(useGovernanceStore.getState().alerts.map(a => a.id)).toEqual(['a2'])
    expect(mockDismiss).toHaveBeenCalledWith('a1')
  })

  it('dismiss keeps the row (stamps dismissedAt) while dismissed alerts are shown', async () => {
    useGovernanceStore.setState({alerts: [baseAlert({id: 'a1'})], showDismissed: true})
    await useGovernanceStore.getState().dismiss('a1')
    expect(useGovernanceStore.getState().alerts[0]!.dismissedAt).toBeTruthy()
  })

  // "Clear dismissed" only reaches alerts the list actually contains. The
  // default list hides dismissed rows, so without includeDismissed an alert
  // dismissed in an earlier session could never be cleared.
  it('the list only asks for dismissed alerts once the toggle is on', async () => {
    await useGovernanceStore.getState().openPanel()
    expect(mockList).toHaveBeenLastCalledWith({limit: 50, includeDismissed: false})

    const dismissed = baseAlert({id: 'old', dismissedAt: '2026-01-01T00:00:00Z'})
    mockList.mockResolvedValue([dismissed])
    await useGovernanceStore.getState().setShowDismissed(true)
    expect(mockList).toHaveBeenLastCalledWith({limit: 50, includeDismissed: true})
    expect(useGovernanceStore.getState().alerts).toEqual([dismissed])

    await useGovernanceStore.getState().clearDismissed()
    expect(mockClear).toHaveBeenCalled()
    expect(useGovernanceStore.getState().alerts).toEqual([])
  })

  it('a reload that resolves after the toggle flipped is discarded', async () => {
    let resolveFirst: (v: GovernanceAlert[]) => void = () => {}
    mockList.mockImplementationOnce(() => new Promise(r => (resolveFirst = r)))
    const first = useGovernanceStore.getState().setShowDismissed(true)
    mockList.mockResolvedValue([])
    await useGovernanceStore.getState().setShowDismissed(false)
    resolveFirst([baseAlert({id: 'stale', dismissedAt: '2026-01-01T00:00:00Z'})])
    await first
    expect(useGovernanceStore.getState().alerts).toEqual([])
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
    useGovernanceStore.setState({alerts: [baseAlert()], unreadCount: 5, panelOpen: true, showDismissed: true})
    useGovernanceStore.getState().reset()
    const st = useGovernanceStore.getState()
    expect(st.alerts).toEqual([])
    expect(st.showDismissed).toBe(false)
    expect(st.unreadCount).toBe(0)
    expect(st.panelOpen).toBe(false)
  })
})

describe('governanceStore unread polling and page visibility', () => {
  let visibility: DocumentVisibilityState = 'visible'

  beforeEach(() => {
    vi.useFakeTimers()
    visibility = 'visible'
    Object.defineProperty(document, 'visibilityState', {configurable: true, get: () => visibility})
  })

  afterEach(() => {
    stopGovernancePolling()
    vi.useRealTimers()
    Reflect.deleteProperty(document, 'visibilityState')
  })

  const becomeVisible = () => {
    visibility = 'visible'
    document.dispatchEvent(new Event('visibilitychange'))
  }

  it('skips the tick while the tab is hidden and refreshes once when it becomes visible', () => {
    visibility = 'hidden'
    startGovernancePolling()
    // The initial fetch still runs (the badge needs a value for when the
    // tab is next shown)...
    expect(mockUnreadCount).toHaveBeenCalledTimes(1)
    // ...but three intervals in the background fetch nothing.
    vi.advanceTimersByTime(180_000)
    expect(mockUnreadCount).toHaveBeenCalledTimes(1)
    // Coming back catches up with one refresh.
    becomeVisible()
    expect(mockUnreadCount).toHaveBeenCalledTimes(2)
    // A visibilitychange that lands on 'hidden' is not a refresh.
    visibility = 'hidden'
    document.dispatchEvent(new Event('visibilitychange'))
    expect(mockUnreadCount).toHaveBeenCalledTimes(2)
  })

  it('keeps polling on the interval while the tab is visible', () => {
    startGovernancePolling()
    expect(mockUnreadCount).toHaveBeenCalledTimes(1)
    vi.advanceTimersByTime(120_000)
    expect(mockUnreadCount).toHaveBeenCalledTimes(3)
  })

  it('stops listening for visibility once polling stops', () => {
    visibility = 'hidden'
    startGovernancePolling()
    stopGovernancePolling()
    becomeVisible()
    vi.advanceTimersByTime(120_000)
    expect(mockUnreadCount).toHaveBeenCalledTimes(1)
  })
})
