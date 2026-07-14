import {create} from 'zustand'
import {authApi, type AuthUser, type LoginRequest} from '@/api/auth'
import {registerRefreshCallback, invalidateConfigCache, setSessionToken} from '@/api/client'
import {decodeJwtPayload} from '@/lib/jwt'
import {resetAllStores} from './storeRegistry'

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

// getCurrentSessionId decodes the `jti` claim from the currently-held refresh
// token, so the UI can mark which entry in the active-sessions list is "this
// device" without any backend change (the session id IS the refresh token's
// jti). Returns null if there is no refresh token or it isn't a decodable JWT.
export function getCurrentSessionId(): string | null {
  const token = readRefresh()
  if (!token) return null
  const payload = decodeJwtPayload(token)
  const jti = payload?.jti
  return typeof jti === 'string' ? jti : null
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
    set({isLoading: true, error: null})
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
    set({isLoading: true, error: null})
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

  loginWithSSOTicket: async ticket => {
    if (get().isLoading) return
    set({isLoading: true, error: null})
    try {
      const res = await authApi.ssoExchange(ticket)
      // SSO sessions persist like "remember me" — the user already chose a
      // long-lived session at their identity provider.
      writeRefresh(res.refreshToken, true)
      setSessionToken(res.accessToken)
      // The exchange response carries only the token pair; resolve the
      // profile explicitly so the UI has email/role immediately.
      const user = res.user ?? (await authApi.me())
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
    set({isLoading: true})
    try {
      await authApi.logout()
    } catch {
      // Best-effort logout — clear local state regardless
    } finally {
      clearTokens()
      set({user: null, accessToken: null, isAuthenticated: false, isLoading: false, error: null})
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
        set({user: null, accessToken: null, isAuthenticated: false})
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
        set({accessToken: res.accessToken})
        return true
      } catch {
        clearTokens()
        set({user: null, accessToken: null, isAuthenticated: false})
        return false
      }
    })().finally(() => {
      refreshInFlight = null
    })

    return refreshInFlight
  },

  loadFromStorage: async () => {
    const refreshToken = readRefresh()
    // No token, or a token already expired by its `exp` claim: go straight to
    // the login form without any network call (avoids a startup 401).
    if (!refreshToken || isJwtExpired(refreshToken)) {
      clearTokens()
      set({user: null, accessToken: null, isAuthenticated: false})
      return
    }

    set({isLoading: true})
    try {
      const ok = await get().refresh()
      if (!ok) {
        set({isLoading: false})
        return
      }
      const user = await authApi.me()
      set({user, isAuthenticated: true, isLoading: false})
    } catch {
      clearTokens()
      set({user: null, accessToken: null, isAuthenticated: false, isLoading: false})
    }
  },

  updateUser: patch => set(s => ({user: s.user ? {...s.user, ...patch} : s.user})),

  clearError: () => set({error: null}),
}))

// Register with the API client so it can transparently refresh expired tokens.
registerRefreshCallback(async () => {
  await useAuthStore.getState().refresh()
  invalidateConfigCache()
})

// resetAllStores tears down every domain store on logout to prevent
// cross-session data leakage when a user logs out — important on a shared
// browser/device where the next user logs in without a full page reload. Stores
// self-register their reset handler via registerStoreReset() in storeRegistry.ts
// (so a new store can't be forgotten here, and authStore needn't import them
// all). syncStore has no handler of its own — it mirrors the SyncManager queue
// and is torn down inside presenceStore's handler (disconnect → syncManager.reset).
// Re-exported so callers/tests can import it from this module as before.
export {resetAllStores}
