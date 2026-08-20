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
  lastError: string | null

  openPanel: () => Promise<void>
  reloadList: () => Promise<void>
  closePanel: () => void
  refreshUnread: () => Promise<void>
  markRead: (id: string) => Promise<void>
  markAllRead: () => Promise<void>
  dismiss: (id: string) => Promise<void>
  clearDismissed: () => Promise<void>
  reset: () => void
}

let pollTimer: ReturnType<typeof setInterval> | null = null

function startPolling(get: () => GovernanceState) {
  if (pollTimer != null) return
  // Immediately fetch once, then on the interval.
  void get().refreshUnread()
  pollTimer = setInterval(() => void get().refreshUnread(), UNREAD_POLL_MS)
}

function stopPolling() {
  if (pollTimer != null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

export const useGovernanceStore = create<GovernanceState>((set, get) => ({
  alerts: [],
  unreadCount: 0,
  loading: false,
  panelOpen: false,
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
      const [alerts] = await Promise.all([governanceApi.list({limit: 50}), governanceApi.markAllRead()])
      set({alerts, unreadCount: 0, loading: false, lastError: null})
    } catch (err) {
      set({loading: false, lastError: err instanceof Error ? err.message : 'Failed to load alerts'})
    }
  },

  // reloadList refreshes the visible list WITHOUT re-acknowledging. Used when a
  // real-time governance:alert SSE event arrives while the panel is already open
  // (the new alert should appear, but we don't want to mark it read on the user's
  // behalf before they've seen it).
  reloadList: async () => {
    try {
      const alerts = await governanceApi.list({limit: 50})
      set({alerts, lastError: null})
    } catch (err) {
      logger.warn('governance: reload list failed', err)
    }
  },

  closePanel: () => set({panelOpen: false}),

  markRead: async (id: string) => {
    const prev = get().alerts
    // Optimistic: stamp a local readAt so the row restyles immediately.
    set({
      alerts: prev.map(a => (a.id === id && !a.readAt ? {...a, readAt: new Date().toISOString()} : a)),
    })
    try {
      await governanceApi.markRead(id)
    } catch (err) {
      set({alerts: prev, lastError: err instanceof Error ? err.message : 'Failed to mark alert'})
    }
  },

  markAllRead: async () => {
    const prev = get()
    // Optimistic: stamp every unread row and zero the badge. On failure,
    // restore the previous alerts + unreadCount (mirroring markRead/dismiss)
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
        lastError: err instanceof Error ? err.message : 'Failed to mark all alerts read',
      })
    }
  },

  dismiss: async (id: string) => {
    const prev = get().alerts
    set({alerts: prev.map(a => (a.id === id ? {...a, dismissedAt: new Date().toISOString()} : a))})
    try {
      await governanceApi.dismiss(id)
    } catch (err) {
      set({alerts: prev, lastError: err instanceof Error ? err.message : 'Failed to dismiss alert'})
    }
  },

  clearDismissed: async () => {
    const prev = get().alerts
    set({alerts: prev.filter(a => !a.dismissedAt)})
    try {
      await governanceApi.clear()
    } catch (err) {
      set({alerts: prev, lastError: err instanceof Error ? err.message : 'Failed to clear alerts'})
    }
  },

  reset: () => {
    stopPolling()
    set({alerts: [], unreadCount: 0, loading: false, panelOpen: false, lastError: null})
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
