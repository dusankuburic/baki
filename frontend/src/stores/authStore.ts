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

  login: (credentials: LoginRequest) => Promise<void>
  register: (credentials: LoginRequest) => Promise<void>
  logout: () => Promise<void>
  refresh: () => Promise<void>
  loadFromStorage: () => Promise<void>
  clearError: () => void
}

function readRefresh(): string | null {
  try {
    return sessionStorage.getItem(REFRESH_TOKEN_KEY)
  } catch {
    return null
  }
}

function writeRefresh(value: string): void {
  try {
    sessionStorage.setItem(REFRESH_TOKEN_KEY, value)
  } catch {
  }
}

function clearTokens(): void {
  try {
    sessionStorage.removeItem(REFRESH_TOKEN_KEY)
    setSessionToken(null)
  } catch {
  }
}

export const useAuthStore = create<AuthState>((set, get) => ({
  user: null,
  accessToken: null,
  isAuthenticated: false,
  isLoading: false,
  error: null,

  login: async (credentials) => {
    set({ isLoading: true, error: null })
    try {
      const res = await authApi.login(credentials)
      writeRefresh(res.refreshToken)
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

  register: async (credentials) => {
    set({ isLoading: true, error: null })
    try {
      const res = await authApi.register(credentials)
      writeRefresh(res.refreshToken)
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
      return
    }

    try {
      const res = await authApi.refresh(refreshToken)
      setSessionToken(res.accessToken)
      set({ accessToken: res.accessToken })
    } catch {
      clearTokens()
      set({ user: null, accessToken: null, isAuthenticated: false })
    }
  },

  loadFromStorage: async () => {
    const refreshToken = readRefresh()
    if (!refreshToken) return

    set({ isLoading: true })
    try {
      await get().refresh()
      const user = await authApi.me()
      set({ user, isAuthenticated: true, isLoading: false })
    } catch {
      set({ isLoading: false })
    }
  },

  clearError: () => set({ error: null }),
}))

// Register with the API client so it can transparently refresh expired tokens.
registerRefreshCallback(async () => {
  await useAuthStore.getState().refresh()
  invalidateConfigCache()
})
