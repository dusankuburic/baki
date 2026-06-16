import { create } from 'zustand'
import { authApi, type AuthUser, type LoginRequest } from '@/api/auth'
import { registerRefreshCallback, invalidateConfigCache, setSessionToken } from '@/api/client'
import { decodeJwtPayload } from '@/lib/jwt'

const REFRESH_TOKEN_KEY = 'auth_refresh_token'

// Session epoch: incremented on every logout. The refresh function checks
// this before writing tokens, preventing an in-flight refresh from
// resurrecting a logged-out session.
let sessionEpoch = 0

interface AuthState {
  user: AuthUser | null
  accessToken: string | null
  isAuthenticated: boolean
  isLoading: boolean
  error: string | null

  login: (credentials: LoginRequest, remember?: boolean) => Promise<void>
  register: (credentials: LoginRequest, remember?: boolean) => Promise<void>
  loginWithSSOTicket: (ticket: string) => Promise<void>
  logout: () => Promise<void>
  refresh: () => Promise<boolean>
  loadFromStorage: () => Promise<void>
  updateUser: (patch: Partial<AuthUser>) => void
  clearError: () => void
}

// isJwtExpired decodes a JWT's `exp` claim and reports whether it is already in
// the past. This lets us skip a doomed /api/auth/refresh call (and its console
// 401) at startup. A token we cannot decode is treated as NOT expired: rather
// than silently discard a possibly-valid session, we let the server's 401 be
// the arbiter. A decodable JWT that carries no `exp` is unusable, so it counts
// as expired.
function isJwtExpired(token: string): boolean {
  const payload = decodeJwtPayload(token)
  if (!payload) return false
  const exp = payload.exp
  if (typeof exp !== 'number') return true
  return exp * 1000 <= Date.now()
}

// The refresh token lives in sessionStorage by default (cleared on tab close).
// With "remember me" it goes to localStorage instead, so the session survives a
// browser restart. We read from both and keep only one populated at a time.
function readRefresh(): string | null {
  try {
    return localStorage.getItem(REFRESH_TOKEN_KEY) ?? sessionStorage.getItem(REFRESH_TOKEN_KEY)
  } catch {
    return null
  }
}

// isPersistent reports whether the current refresh token is stored persistently
// (localStorage), so a rotated token is re-saved to the same place.
function isPersistent(): boolean {
  try {
    return localStorage.getItem(REFRESH_TOKEN_KEY) !== null
  } catch {
    return false
  }
}

function writeRefresh(value: string, persistent: boolean): void {
  try {
    if (persistent) {
      localStorage.setItem(REFRESH_TOKEN_KEY, value)
      sessionStorage.removeItem(REFRESH_TOKEN_KEY)
    } else {
      sessionStorage.setItem(REFRESH_TOKEN_KEY, value)
      localStorage.removeItem(REFRESH_TOKEN_KEY)
    }
  } catch {
    // storage may be unavailable (private mode / disabled) — ignore.
  }
}

function clearTokens(): void {
  try {
    sessionStorage.removeItem(REFRESH_TOKEN_KEY)
    localStorage.removeItem(REFRESH_TOKEN_KEY)
    setSessionToken(null)
  } catch {
    // storage may be unavailable — best-effort clear.
  }
}

let refreshInFlight: Promise<boolean> | null = null

export const useAuthStore = create<AuthState>((set, get) => ({
  user: null,
  accessToken: null,
  isAuthenticated: false,
  isLoading: false,
  error: null,

  login: async (credentials, remember = false) => {
    if (get().isLoading) return
    set({ isLoading: true, error: null })
    try {
      const res = await authApi.login(credentials)
      writeRefresh(res.refreshToken, remember)
      setSessionToken(res.accessToken)
      set({
        user: res.user,
        accessToken: res.accessToken,
        isAuthenticated: true,
        isLoading: false,
      })
    } catch (err) {
      set({
        isLoading: false,
        error: err instanceof Error ? err.message : 'Login failed',
      })
      throw err
    }
  },

  register: async (credentials, remember = false) => {
    if (get().isLoading) return
    set({ isLoading: true, error: null })
    try {
      const res = await authApi.register(credentials)
      writeRefresh(res.refreshToken, remember)
      setSessionToken(res.accessToken)
      set({
        user: res.user,
        accessToken: res.accessToken,
        isAuthenticated: true,
        isLoading: false,
      })
    } catch (err) {
      set({
        isLoading: false,
        error: err instanceof Error ? err.message : 'Registration failed',
      })
      throw err
    }
  },

  loginWithSSOTicket: async (ticket) => {
    if (get().isLoading) return
    set({ isLoading: true, error: null })
    try {
      const res = await authApi.ssoExchange(ticket)
      // SSO sessions persist like "remember me" — the user already chose a
      // long-lived session at their identity provider.
      writeRefresh(res.refreshToken, true)
      setSessionToken(res.accessToken)
      // The exchange response carries only the token pair; resolve the
      // profile explicitly so the UI has email/role immediately.
      const user = res.user ?? await authApi.me()
      set({
        user,
        accessToken: res.accessToken,
        isAuthenticated: true,
        isLoading: false,
      })
    } catch (err) {
      set({
        isLoading: false,
        error: err instanceof Error ? err.message : 'SSO login failed',
      })
      throw err
    }
  },

  logout: async () => {
    // Increment epoch so any in-flight refresh won't write new tokens after logout.
    sessionEpoch++
    set({ isLoading: true })
    try {
      await authApi.logout()
    } catch {
      // Best-effort logout — clear local state regardless
    } finally {
      clearTokens()
      set({ user: null, accessToken: null, isAuthenticated: false, isLoading: false, error: null })
      // Reset all domain stores + tear down live connections to prevent
      // cross-session data leakage. Awaited so a caller knows teardown completed.
      await resetAllStores()
    }
  },

  refresh: async () => {
    if (refreshInFlight) return refreshInFlight
    const myEpoch = sessionEpoch

    refreshInFlight = (async () => {
      const refreshToken = readRefresh()
      if (!refreshToken) {
        clearTokens()
        set({ user: null, accessToken: null, isAuthenticated: false })
        return false
      }

      try {
        const res = await authApi.refresh(refreshToken)
        // Guard against post-logout resurrection: if the user logged out
        // while this refresh was in flight, discard the new tokens.
        if (myEpoch !== sessionEpoch) {
          return false
        }
        setSessionToken(res.accessToken)
        if (res.refreshToken) {
          writeRefresh(res.refreshToken, isPersistent())
        }
        set({ accessToken: res.accessToken })
        return true
      } catch {
        clearTokens()
        set({ user: null, accessToken: null, isAuthenticated: false })
        return false
      }
    })().finally(() => { refreshInFlight = null })

    return refreshInFlight
  },

  loadFromStorage: async () => {
    const refreshToken = readRefresh()
    // No token, or a token already expired by its `exp` claim: go straight to
    // the login form without any network call (avoids a startup 401).
    if (!refreshToken || isJwtExpired(refreshToken)) {
      clearTokens()
      set({ user: null, accessToken: null, isAuthenticated: false })
      return
    }

    set({ isLoading: true })
    try {
      const ok = await get().refresh()
      if (!ok) {
        set({ isLoading: false })
        return
      }
      const user = await authApi.me()
      set({ user, isAuthenticated: true, isLoading: false })
    } catch {
      clearTokens()
      set({ user: null, accessToken: null, isAuthenticated: false, isLoading: false })
    }
  },

  updateUser: (patch) => set(s => ({ user: s.user ? { ...s.user, ...patch } : s.user })),

  clearError: () => set({ error: null }),
}))

// Register with the API client so it can transparently refresh expired tokens.
registerRefreshCallback(async () => {
  await useAuthStore.getState().refresh()
  invalidateConfigCache()
})

// resetAllStores clears every domain store and tears down live session side
// effects (the collaboration WebSocket and the offline sync queue) to prevent
// cross-session data leakage when a user logs out — important on a shared
// browser/device where the next user logs in without a full page reload.
//
// Invariant (see AGENTS.md "Add a new frontend store"): every store under
// stores/ must be reset here. (syncStore has no explicit entry — it mirrors the
// SyncManager queue and is reset transitively by the presenceStore/syncManager
// teardown below; don't remove that teardown assuming syncStore is covered
// directly.) Lazy imports avoid circular dependencies; each is guarded so one
// failure can't abort the rest (or the logout it runs from).
export async function resetAllStores(): Promise<void> {
  const guard = (p: Promise<unknown>) => p.catch(() => {})
  await Promise.all([
    guard(import('@/stores/flowStore').then(m => m.useFlowStore.getState().reset())),
    guard(import('@/stores/chatStore').then(m => {
      m.useChatStore.setState({
        threads: [],
        activeThreadId: null,
        conversations: new Map(),
        activeStreamId: null,
        streamingMessageId: null,
        streamingText: '',
        pendingMessage: null,
        selectedProvider: 'claude',
      })
    })),
    guard(import('@/stores/analysisStore').then(m => m.useAnalysisStore.getState().reset())),
    guard(import('@/stores/orgStore').then(m => {
      m.useOrgStore.setState({ organisations: [], activeOrgId: null, isLoading: false, isBusy: false, error: null })
    })),
    // Closes the collaboration WebSocket and discards the offline sync queue
    // (including its localStorage copy) so the next user on a shared device can't
    // inherit the previous user's pending mutations. reset() must run AFTER
    // disconnect()→stop() (which re-persists the queue), so chain them rather
    // than rely on Promise.all ordering.
    guard(import('@/stores/presenceStore').then(async m => {
      m.usePresenceStore.getState().disconnect()
      const { syncManager } = await import('@/services/sync/SyncManager')
      syncManager.reset()
    })),
    // searchStore.results carries flow-content matches — clear them.
    guard(import('@/stores/searchStore').then(m => m.useSearchStore.getState().clear())),
    guard(import('@/stores/libraryBrowseStore').then(m => m.useLibraryBrowseStore.getState().reset())),
    guard(import('@/stores/editorStore').then(m => m.useEditorStore.setState({
      groups: [{ tabs: [], activeTabId: null }], focusedGroupIndex: 0, groupWidths: [100],
    }))),
    // Settings repopulate from the backend on the next login (loadFromBackend).
    guard(import('@/stores/settingsStore').then(m => m.useSettingsStore.setState({
      settings: m.defaultSettings, isLoaded: false,
    }))),
    // Reset UI chrome and the diff view (activeDiff holds flow content), but keep
    // resolvedTheme so the login screen doesn't flash before settings reload.
    guard(import('@/stores/uiStore').then(m => m.useUIStore.setState({
      sidebarTab: 'explorer', mainPaneView: 'home', inspectorTab: 'details',
      sidebarCollapsed: false, inspectorCollapsed: false, commandPaletteOpen: false,
      globalSearchOpen: false, complexityMode: false, settingsOpen: false,
      variablePanelOpen: false, selectedVariable: null, graphZoom: 1, activeDiff: null,
    }))),
  ])
}
