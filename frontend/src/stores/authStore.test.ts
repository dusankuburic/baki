import { describe, it, expect, vi, beforeEach } from 'vitest'
import { useAuthStore } from './authStore'
import type { AuthUser, LoginResponse } from '@/api/auth'

vi.mock('@/api/auth', () => ({
  authApi: {
    login:   vi.fn(),
    logout:  vi.fn(),
    refresh: vi.fn(),
    me:      vi.fn(),
  },
}))

// Also mock the platform adapter so the api/client module doesn't try to
// invoke Tauri during tests.
vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({
      apiUrl: 'http://localhost:9999',
      token: 'mock-token',
    }),
  }),
}))

import { authApi } from '@/api/auth'

const mockLogin   = authApi.login   as ReturnType<typeof vi.fn>
const mockLogout  = authApi.logout  as ReturnType<typeof vi.fn>
const mockRefresh = authApi.refresh as ReturnType<typeof vi.fn>
const mockMe      = authApi.me      as ReturnType<typeof vi.fn>

const fakeUser: AuthUser = { id: 'u1', email: 'alice@example.com', role: 'member' }

const fakeLoginResponse: LoginResponse = {
  accessToken:  'access-abc',
  refreshToken: 'refresh-xyz',
  expiresAt:    new Date(Date.now() + 900_000).toISOString(),
  user:          fakeUser,
}

// jsdom's localStorage implementation in vitest 4 is incomplete — stub the whole thing.
let _storage: Record<string, string> = {}
vi.stubGlobal('localStorage', {
  getItem:    (key: string) => _storage[key] ?? null,
  setItem:    (key: string, value: string) => { _storage[key] = value },
  removeItem: (key: string) => { delete _storage[key] },
})

const initialState = useAuthStore.getState()

beforeEach(() => {
  useAuthStore.setState(initialState, true)
  _storage = {}
  vi.resetAllMocks()
  mockLogout.mockResolvedValue(undefined)
})

// ---- login ----

describe('login', () => {
  it('sets isAuthenticated and user on success', async () => {
    mockLogin.mockResolvedValue(fakeLoginResponse)

    await useAuthStore.getState().login({ email: 'alice@example.com', password: 'secret' })

    const s = useAuthStore.getState()
    expect(s.isAuthenticated).toBe(true)
    expect(s.user).toEqual(fakeUser)
    expect(s.accessToken).toBe('access-abc')
    expect(s.isLoading).toBe(false)
    expect(s.error).toBeNull()
  })

  it('stores tokens in localStorage', async () => {
    mockLogin.mockResolvedValue(fakeLoginResponse)
    await useAuthStore.getState().login({ email: 'a@b.com', password: 'p' })

    expect(localStorage.getItem('auth_access_token')).toBe('access-abc')
    expect(localStorage.getItem('auth_refresh_token')).toBe('refresh-xyz')
  })

  it('sets error and rethrows on failure', async () => {
    mockLogin.mockRejectedValue(new Error('Invalid credentials'))

    await expect(
      useAuthStore.getState().login({ email: 'a@b.com', password: 'wrong' })
    ).rejects.toThrow('Invalid credentials')

    const s = useAuthStore.getState()
    expect(s.isAuthenticated).toBe(false)
    expect(s.error).toBe('Invalid credentials')
    expect(s.isLoading).toBe(false)
  })
})

// ---- logout ----

describe('logout', () => {
  it('clears user and token after logout', async () => {
    // Seed logged-in state
    useAuthStore.setState({ user: fakeUser, accessToken: 'tok', isAuthenticated: true })

    await useAuthStore.getState().logout()

    const s = useAuthStore.getState()
    expect(s.user).toBeNull()
    expect(s.accessToken).toBeNull()
    expect(s.isAuthenticated).toBe(false)
  })

  it('clears localStorage tokens', async () => {
    localStorage.setItem('auth_access_token', 'tok')
    localStorage.setItem('auth_refresh_token', 'ref')

    await useAuthStore.getState().logout()

    expect(localStorage.getItem('auth_access_token')).toBeNull()
    expect(localStorage.getItem('auth_refresh_token')).toBeNull()
  })

  it('clears state even when the API call fails', async () => {
    mockLogout.mockRejectedValue(new Error('server error'))
    useAuthStore.setState({ user: fakeUser, isAuthenticated: true })

    await useAuthStore.getState().logout()

    expect(useAuthStore.getState().isAuthenticated).toBe(false)
  })
})

// ---- refresh ----

describe('refresh', () => {
  it('updates the access token on success', async () => {
    localStorage.setItem('auth_refresh_token', 'refresh-xyz')
    mockRefresh.mockResolvedValue({ accessToken: 'new-access', expiresAt: new Date().toISOString() })

    await useAuthStore.getState().refresh()

    expect(useAuthStore.getState().accessToken).toBe('new-access')
    expect(localStorage.getItem('auth_access_token')).toBe('new-access')
  })

  it('clears auth when no refresh token is stored', async () => {
    useAuthStore.setState({ user: fakeUser, isAuthenticated: true })

    await useAuthStore.getState().refresh()

    expect(useAuthStore.getState().isAuthenticated).toBe(false)
  })

  it('clears auth when refresh API fails', async () => {
    localStorage.setItem('auth_refresh_token', 'expired-refresh')
    mockRefresh.mockRejectedValue(new Error('expired'))
    useAuthStore.setState({ user: fakeUser, isAuthenticated: true })

    await useAuthStore.getState().refresh()

    expect(useAuthStore.getState().isAuthenticated).toBe(false)
  })
})

// ---- loadFromStorage ----

describe('loadFromStorage', () => {
  it('restores session when a valid token is in localStorage', async () => {
    localStorage.setItem('auth_access_token', 'stored-token')
    mockMe.mockResolvedValue(fakeUser)

    await useAuthStore.getState().loadFromStorage()

    expect(useAuthStore.getState().isAuthenticated).toBe(true)
    expect(useAuthStore.getState().user).toEqual(fakeUser)
  })

  it('does nothing when no token is stored', async () => {
    await useAuthStore.getState().loadFromStorage()
    expect(useAuthStore.getState().isAuthenticated).toBe(false)
    expect(mockMe).not.toHaveBeenCalled()
  })
})

// ---- clearError ----

describe('clearError', () => {
  it('resets the error field', () => {
    useAuthStore.setState({ error: 'some error' })
    useAuthStore.getState().clearError()
    expect(useAuthStore.getState().error).toBeNull()
  })
})
