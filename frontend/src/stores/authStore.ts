import { create } from 'zustand'
import { authApi, type AuthUser, type LoginRequest } from '@/api/auth'
import { registerRefreshCallback, invalidateConfigCache, setSessionToken } from '@/api/client'

const REFRESH_TOKEN_KEY = 'auth_refresh_token'

interface AuthState {
  user: AuthUser | null
  accessToken: string | null
  isAuthenticated: boolean
  isLoading: boolean
  error: string | null

  login: (credentials: LoginRequest, remember?: boolean) => Promise<void>
  register: (credentials: LoginRequest, remember?: boolean) => Promise<void>
  logout: () => Promise<void>
  refresh: () => Promise<boolean>
  loadFromStorage: () => Promise<void>
  clearError: () => void
}

// isJwtExpired decodes a JWT's `exp` claim and reports whether it is missing or
// already in the past. Malformed tokens are treated as expired. This lets us
// skip a doomed /api/auth/refresh call (and its console 401) at startup.
function isJwtExpired(token: string): boolean {
  try {
    const payload = JSON.parse(atob(token.split('.')[1]))
    if (typeof payload.exp !== 'number') return true
    return payload.exp * 1000 <= Date.now()
  } catch {
    return true
  }
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

export const useAuthStore = create<AuthState>((set, get) => ({
  user: null,
  accessToken: null,
  isAuthenticated: false,
  isLoading: false,
  error: null,

  login: async (credentials, remember = false) => {
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

  logout: async () => {
    set({ isLoading: true })
    try {
      await authApi.logout()
    } catch {
      // Best-effort logout — clear local state regardless
    } finally {
      clearTokens()
      set({ user: null, accessToken: null, isAuthenticated: false, isLoading: false, error: null })
    }
  },

  refresh: async () => {
    const refreshToken = readRefresh()
    if (!refreshToken) {
      clearTokens()
      set({ user: null, accessToken: null, isAuthenticated: false })
      return false
    }

    try {
      const res = await authApi.refresh(refreshToken)
      setSessionToken(res.accessToken)
      // With rotation the server returns a fresh refresh token; persist it to the
      // same store the old one came from so the next refresh uses the new token.
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

  clearError: () => set({ error: null }),
}))

// Register with the API client so it can transparently refresh expired tokens.
registerRefreshCallback(async () => {
  await useAuthStore.getState().refresh()
  invalidateConfigCache()
})
