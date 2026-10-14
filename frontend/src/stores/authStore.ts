import {getErrorMessage} from '@/lib/getErrorMessage'
import {create} from 'zustand'
import {authApi, type AuthUser, type LoginRequest} from '@/api/auth'
import {registerRefreshCallback, invalidateConfigCache, setSessionToken} from '@/api/client'
import {clearAvatarCache} from '@/lib/avatarCache'
import {decodeJwtPayload} from '@/lib/jwt'
import {isTauri} from '@/platform/guards'
import {resetAllStores, registerCurrentUserResolver} from './storeRegistry'

const REFRESH_TOKEN_KEY = 'auth_refresh_token'

const SESSION_MARKER_KEY = 'auth_session_active'

function readSessionMarker(): 'p' | '1' | null {
  if (isTauri()) return null
  try {
    const v = localStorage.getItem(SESSION_MARKER_KEY)
    return v === 'p' || v === '1' ? v : null
  } catch {
    return null
  }
}

function writeSessionMarker(persistent: boolean): void {
  try {
    localStorage.setItem(SESSION_MARKER_KEY, persistent ? 'p' : '1')
  } catch {
  }
}

function clearSessionMarker(): void {
  try {
    localStorage.removeItem(SESSION_MARKER_KEY)
  } catch {
  }
}

function readLegacyRefresh(): string | null {
  try {
    return localStorage.getItem(REFRESH_TOKEN_KEY) ?? sessionStorage.getItem(REFRESH_TOKEN_KEY)
  } catch {
    return null
  }
}

let webRefreshJti: string | null = null

function jtiOf(token: string | undefined): string | null {
  if (!token) return null
  const payload = decodeJwtPayload(token)
  return typeof payload?.jti === 'string' ? payload.jti : null
}

let sessionEpoch = 0

export type RegisterOutcome = 'signed-in' | 'verification-required'

interface AuthState {
  user: AuthUser | null
  accessToken: string | null
  isAuthenticated: boolean
  isLoading: boolean
  isBooting: boolean
  error: string | null

  login: (credentials: LoginRequest, remember?: boolean) => Promise<void>
  register: (credentials: LoginRequest, remember?: boolean) => Promise<RegisterOutcome>
  loginWithSSOTicket: (ticket: string) => Promise<void>
  logout: () => Promise<void>
  refresh: () => Promise<boolean>
  loadFromStorage: () => Promise<void>
  updateUser: (patch: Partial<AuthUser>) => void
  clearError: () => void
}

function isJwtExpired(token: string): boolean {
  const payload = decodeJwtPayload(token)
  if (!payload) return false
  const exp = payload.exp
  if (typeof exp !== 'number') return true
  return exp * 1000 <= Date.now()
}

function readRefresh(): string | null {
  if (!isTauri()) return null
  try {
    return localStorage.getItem(REFRESH_TOKEN_KEY) ?? sessionStorage.getItem(REFRESH_TOKEN_KEY)
  } catch {
    return null
  }
}

function isPersistent(): boolean {
  try {
    return localStorage.getItem(REFRESH_TOKEN_KEY) !== null
  } catch {
    return false
  }
}

function writeRefresh(value: string, persistent: boolean): void {
  if (!isTauri()) {
    writeSessionMarker(persistent)
    webRefreshJti = jtiOf(value)
    return
  }
  try {
    if (persistent) {
      localStorage.setItem(REFRESH_TOKEN_KEY, value)
      sessionStorage.removeItem(REFRESH_TOKEN_KEY)
    } else {
      sessionStorage.setItem(REFRESH_TOKEN_KEY, value)
      localStorage.removeItem(REFRESH_TOKEN_KEY)
    }
  } catch {
  }
}

export function getCurrentSessionId(): string | null {
  const token = readRefresh()
  if (token) {
    const payload = decodeJwtPayload(token)
    return typeof payload?.jti === 'string' ? payload.jti : null
  }
  return webRefreshJti
}

function clearTokens(): void {
  clearSessionMarker()
  webRefreshJti = null
  try {
    sessionStorage.removeItem(REFRESH_TOKEN_KEY)
    localStorage.removeItem(REFRESH_TOKEN_KEY)
    setSessionToken(null)
  } catch {
  }
}

let refreshInFlight: Promise<boolean> | null = null

export const useAuthStore = create<AuthState>((set, get) => ({
  user: null,
  accessToken: null,
  isAuthenticated: false,
  isLoading: false,
  isBooting: false,
  error: null,

  login: async (credentials, remember = false) => {
    if (get().isLoading) return
    set({isLoading: true, error: null})
    try {
      const res = await authApi.login({...credentials, rememberMe: remember})
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
        error: getErrorMessage(err, 'Login failed'),
      })
      throw err
    }
  },

  register: async (credentials, remember = false) => {
    if (get().isLoading) return 'verification-required'
    set({isLoading: true, error: null})
    try {
      const res = await authApi.register({...credentials, rememberMe: remember})
      if (res.verificationRequired || !res.accessToken || !res.refreshToken) {
        set({isLoading: false})
        return 'verification-required'
      }
      writeRefresh(res.refreshToken, remember)
      setSessionToken(res.accessToken)
      set({
        user: res.user,
        accessToken: res.accessToken,
        isAuthenticated: true,
        isLoading: false,
      })
      return 'signed-in'
    } catch (err) {
      set({
        isLoading: false,
        error: getErrorMessage(err, 'Registration failed'),
      })
      throw err
    }
  },

  loginWithSSOTicket: async ticket => {
    if (get().isLoading) return
    set({isLoading: true, error: null})
    try {
      const res = await authApi.ssoExchange(ticket)
      writeRefresh(res.refreshToken, true)
      setSessionToken(res.accessToken)
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
        error: getErrorMessage(err, 'SSO login failed'),
      })
      throw err
    }
  },

  logout: async () => {
    sessionEpoch++
    set({isLoading: true})
    try {
      await authApi.logout()
    } catch {
    } finally {
      clearTokens()
      // Other users' cached avatar images (bearer-fetched object URLs) must not survive the session.
      clearAvatarCache()
      set({user: null, accessToken: null, isAuthenticated: false, isLoading: false, error: null})
      try {
        navigator.serviceWorker?.controller?.postMessage({type: 'PURGE_CACHES'})
      } catch {
      }
      await resetAllStores()
    }
  },

  refresh: async () => {
    if (refreshInFlight) return refreshInFlight
    const myEpoch = sessionEpoch

    refreshInFlight = (async () => {
      if (!isTauri()) {
        const persistent = readSessionMarker() === 'p'
        if (!readSessionMarker()) {
          clearTokens()
          set({user: null, accessToken: null, isAuthenticated: false})
          return false
        }
        try {
          const res = await authApi.refresh(null, persistent)
          if (myEpoch !== sessionEpoch) return false
          setSessionToken(res.accessToken)
          if (res.refreshToken) webRefreshJti = jtiOf(res.refreshToken)
          set({accessToken: res.accessToken})
          return true
        } catch {
          clearTokens()
          set({user: null, accessToken: null, isAuthenticated: false})
          return false
        }
      }

      const refreshToken = readRefresh()
      if (!refreshToken) {
        clearTokens()
        set({user: null, accessToken: null, isAuthenticated: false})
        return false
      }

      try {
        const res = await authApi.refresh(refreshToken, isPersistent())
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
    if (!isTauri()) {
      const marker = readSessionMarker()
      const legacy = readLegacyRefresh()
      if (!marker && !legacy) {
        set({user: null, accessToken: null, isAuthenticated: false})
        return
      }
      if (!marker) writeSessionMarker(false)
      try {
        sessionStorage.removeItem(REFRESH_TOKEN_KEY)
        localStorage.removeItem(REFRESH_TOKEN_KEY)
      } catch {
      }
      set({isBooting: true})
      try {
        const ok = await get().refresh()
        if (!ok) {
          set({isBooting: false})
          return
        }
        const user = await authApi.me()
        set({user, isAuthenticated: true, isBooting: false})
      } catch {
        clearTokens()
        set({user: null, accessToken: null, isAuthenticated: false, isBooting: false})
      }
      return
    }

    const refreshToken = readRefresh()
    if (!refreshToken || isJwtExpired(refreshToken)) {
      clearTokens()
      set({user: null, accessToken: null, isAuthenticated: false})
      return
    }

    set({isBooting: true})
    try {
      const ok = await get().refresh()
      if (!ok) {
        set({isBooting: false})
        return
      }
      const user = await authApi.me()
      set({user, isAuthenticated: true, isBooting: false})
    } catch {
      clearTokens()
      set({user: null, accessToken: null, isAuthenticated: false, isBooting: false})
    }
  },

  updateUser: patch => set(s => ({user: s.user ? {...s.user, ...patch} : s.user})),

  clearError: () => set({error: null}),
}))

registerCurrentUserResolver(() => useAuthStore.getState().user?.id ?? null)

registerRefreshCallback(async () => {
  await useAuthStore.getState().refresh()
  invalidateConfigCache()
})

export {resetAllStores}
