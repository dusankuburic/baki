import {getErrorMessage} from '@/lib/getErrorMessage'
import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import {governanceApi, type GovernanceAlert} from '@/api/governance'
import {isTauri} from '@/platform/guards'
import {logger} from '@/lib/logger'

const UNREAD_POLL_MS = 60_000

interface GovernanceState {
  alerts: GovernanceAlert[]
  unreadCount: number
  loading: boolean
  panelOpen: boolean
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

function startPolling(get: () => GovernanceState) {
  if (pollTimer != null) return
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
      logger.warn('governance: unread-count fetch failed', err)
    }
  },

  openPanel: async () => {
    set({panelOpen: true, loading: true})
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

  reloadList: async () => {
    const includeDismissed = get().showDismissed
    try {
      const alerts = await governanceApi.list({limit: 50, includeDismissed})
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

export function startGovernancePolling() {
  if (isTauri()) return
  startPolling(() => useGovernanceStore.getState())
}

export function stopGovernancePolling() {
  stopPolling()
}

registerStoreReset(() => useGovernanceStore.getState().reset())
