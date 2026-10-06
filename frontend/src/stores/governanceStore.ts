import {getErrorMessage} from '@/lib/getErrorMessage'
import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import {governanceApi, type GovernanceAlert} from '@/api/governance'
import {isTauri} from '@/platform/guards'
import {logger} from '@/lib/logger'

// Governance-alerts inbox for the notifications bell. The scanner writes alerts
// (drift / health regression) to the backend; this store polls the unread count
// on a slow interval and loads the full list lazily when the bell panel opens.
// Local/desktop mode (isTauri) has no scanner, so the store stays dormant.

const UNREAD_POLL_MS = 60_000

interface GovernanceState {
  alerts: GovernanceAlert[]
  unreadCount: number
  loading: boolean
  panelOpen: boolean
  // Dismissed alerts stay on the server until cleared, but the default list
  // hides them. Toggling this on reloads with includeDismissed so a later
  // session can still find — and clear — what an earlier one dismissed.
  showDismissed: boolean
  lastError: string | null

  openPanel: () => Promise<void>
  reloadList: () => Promise<void>
  closePanel: () => void
  refreshUnread: () => Promise<void>
  setShowDismissed: (show: boolean) => Promise<void>
  markAllRead: () => Promise<void>
  dismiss: (id: string) => Promise<void>
  clearDismissed: () => Promise<void>
  reset: () => void
}

let pollTimer: ReturnType<typeof setInterval> | null = null
let onVisible: (() => void) | null = null

function pageHidden(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'hidden'
}

// The poll is a safety net under the SSE push (governance:alert → refreshUnread),
// so a background tab gets nothing from it: the tick is skipped while the page
// is hidden, and one refresh runs when the tab becomes visible again to
// catch up on anything the skipped ticks would have shown.
function startPolling(get: () => GovernanceState) {
  if (pollTimer != null) return
  // Immediately fetch once, then on the interval.
  void get().refreshUnread()
  pollTimer = setInterval(() => {
    if (pageHidden()) return
    void get().refreshUnread()
  }, UNREAD_POLL_MS)
  if (typeof document !== 'undefined') {
    onVisible = () => {
      if (!pageHidden()) void get().refreshUnread()
    }
    document.addEventListener('visibilitychange', onVisible)
  }
}

function stopPolling() {
  if (pollTimer != null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
  if (onVisible != null) {
    document.removeEventListener('visibilitychange', onVisible)
    onVisible = null
  }
}

export const useGovernanceStore = create<GovernanceState>((set, get) => ({
  alerts: [],
  unreadCount: 0,
  loading: false,
  panelOpen: false,
  showDismissed: false,
  lastError: null,

  refreshUnread: async () => {
    if (isTauri()) return
    try {
      const {count} = await governanceApi.unreadCount()
      set({unreadCount: count, lastError: null})
    } catch (err) {
      // Best-effort poll: don't surface transient errors to the user, just log.
      logger.warn('governance: unread-count fetch failed', err)
    }
  },

  openPanel: async () => {
    set({panelOpen: true, loading: true})
    // Loading the list implicitly acknowledges (clears the badge) — a user who
    // opens the inbox has seen the alerts.
    try {
      const [alerts] = await Promise.all([
        governanceApi.list({limit: 50, includeDismissed: get().showDismissed}),
        governanceApi.markAllRead(),
      ])
      set({alerts, unreadCount: 0, loading: false, lastError: null})
    } catch (err) {
      set({loading: false, lastError: getErrorMessage(err, 'Failed to load alerts')})
    }
  },

  // reloadList refreshes the visible list WITHOUT re-acknowledging. Used when a
  // real-time governance:alert SSE event arrives while the panel is already open
  // (the new alert should appear, but we don't want to mark it read on the user's
  // behalf before they've seen it).
  reloadList: async () => {
    const includeDismissed = get().showDismissed
    try {
      const alerts = await governanceApi.list({limit: 50, includeDismissed})
      // A toggle flipped while this was in flight owns the list now.
      if (get().showDismissed !== includeDismissed) return
      set({alerts, lastError: null})
    } catch (err) {
      logger.warn('governance: reload list failed', err)
    }
  },

  closePanel: () => set({panelOpen: false}),

  setShowDismissed: async (show: boolean) => {
    if (get().showDismissed === show) return
    set({showDismissed: show})
    await get().reloadList()
  },

  markAllRead: async () => {
    const prev = get()
    // Optimistic: stamp every unread row and zero the badge. On failure,
    // restore the previous alerts + unreadCount (mirroring dismiss)
    // so the bell doesn't claim 0 unread while the server disagrees.
    set({
      alerts: prev.alerts.map(a => (a.readAt ? a : {...a, readAt: new Date().toISOString()})),
      unreadCount: 0,
    })
    try {
      await governanceApi.markAllRead()
    } catch (err) {
      set({
        alerts: prev.alerts,
        unreadCount: prev.unreadCount,
        lastError: getErrorMessage(err, 'Failed to mark all alerts read'),
      })
    }
  },

  dismiss: async (id: string) => {
    const {alerts: prev, showDismissed} = get()
    // Mirror what the next list load returns: with dismissed alerts shown the
    // row stays (dimmed, clearable); otherwise it leaves the list.
    set({
      alerts: showDismissed
        ? prev.map(a => (a.id === id ? {...a, dismissedAt: new Date().toISOString()} : a))
        : prev.filter(a => a.id !== id),
    })
    try {
      await governanceApi.dismiss(id)
    } catch (err) {
      set({alerts: prev, lastError: getErrorMessage(err, 'Failed to dismiss alert')})
    }
  },

  clearDismissed: async () => {
    const prev = get().alerts
    set({alerts: prev.filter(a => !a.dismissedAt)})
    try {
      await governanceApi.clear()
    } catch (err) {
      set({alerts: prev, lastError: getErrorMessage(err, 'Failed to clear alerts')})
    }
  },

  reset: () => {
    stopPolling()
    set({alerts: [], unreadCount: 0, loading: false, panelOpen: false, showDismissed: false, lastError: null})
  },
}))

// Start polling once authenticated (idempotent). Called from the bell mount +
// re-auth path. Safe to call in local mode (refreshUnread early-returns).
export function startGovernancePolling() {
  if (isTauri()) return
  startPolling(() => useGovernanceStore.getState())
}

export function stopGovernancePolling() {
  stopPolling()
}

registerStoreReset(() => useGovernanceStore.getState().reset())
