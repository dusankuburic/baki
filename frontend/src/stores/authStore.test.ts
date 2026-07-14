import {describe, it, expect, vi, beforeEach} from 'vitest'
import {useAuthStore} from './authStore'
import type {AuthUser, LoginResponse} from '@/api/auth'

vi.mock('@/api/auth', () => ({
  authApi: {
    login: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
    me: vi.fn(),
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

import {authApi} from '@/api/auth'

const mockLogin = authApi.login as ReturnType<typeof vi.fn>
const mockLogout = authApi.logout as ReturnType<typeof vi.fn>
const mockRefresh = authApi.refresh as ReturnType<typeof vi.fn>
const mockMe = authApi.me as ReturnType<typeof vi.fn>

const fakeUser: AuthUser = {id: 'u1', email: 'alice@example.com', role: 'member'}

// Build a token whose middle segment decodes to a JWT payload with the given
// `exp` (seconds). loadFromStorage decodes this to decide whether to skip a
// doomed refresh call. Default: 15 minutes in the future (valid).
function makeJwt(expMsFromNow = 900_000): string {
  const payload = btoa(JSON.stringify({exp: Math.floor((Date.now() + expMsFromNow) / 1000)}))
  return `header.${payload}.sig`
}

const fakeLoginResponse: LoginResponse = {
  accessToken: 'access-abc',
  refreshToken: 'refresh-xyz',
  expiresAt: new Date(Date.now() + 900_000).toISOString(),
  user: fakeUser,
}

// jsdom's storage implementations in vitest 4 are incomplete — stub them.
// The store keeps the refresh token in sessionStorage and the access token
// in memory (via setSessionToken), never in localStorage.
let _local: Record<string, string> = {}
vi.stubGlobal('localStorage', {
  getItem: (key: string) => _local[key] ?? null,
  setItem: (key: string, value: string) => {
    _local[key] = value
  },
  removeItem: (key: string) => {
    delete _local[key]
  },
})

let _session: Record<string, string> = {}
vi.stubGlobal('sessionStorage', {
  getItem: (key: string) => _session[key] ?? null,
  setItem: (key: string, value: string) => {
    _session[key] = value
  },
  removeItem: (key: string) => {
    delete _session[key]
  },
})

const initialState = useAuthStore.getState()

beforeEach(() => {
  useAuthStore.setState(initialState, true)
  _local = {}
  _session = {}
  vi.resetAllMocks()
  mockLogout.mockResolvedValue(undefined)
})

// ---- login ----

describe('login', () => {
  it('sets isAuthenticated and user on success', async () => {
    mockLogin.mockResolvedValue(fakeLoginResponse)

    await useAuthStore.getState().login({email: 'alice@example.com', password: 'secret'})

    const s = useAuthStore.getState()
    expect(s.isAuthenticated).toBe(true)
    expect(s.user).toEqual(fakeUser)
    expect(s.accessToken).toBe('access-abc')
    expect(s.isLoading).toBe(false)
    expect(s.error).toBeNull()
  })

  it('stores the refresh token in sessionStorage and the access token in state', async () => {
    mockLogin.mockResolvedValue(fakeLoginResponse)
    await useAuthStore.getState().login({email: 'a@b.com', password: 'p'})

    // Access token lives in memory/state only — never in web storage (XSS hardening).
    expect(useAuthStore.getState().accessToken).toBe('access-abc')
    expect(localStorage.getItem('auth_access_token')).toBeNull()
    expect(sessionStorage.getItem('auth_refresh_token')).toBe('refresh-xyz')
  })

  it('sets error and rethrows on failure', async () => {
    mockLogin.mockRejectedValue(new Error('Invalid credentials'))

    await expect(useAuthStore.getState().login({email: 'a@b.com', password: 'wrong'})).rejects.toThrow(
      'Invalid credentials',
    )

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
    useAuthStore.setState({user: fakeUser, accessToken: 'tok', isAuthenticated: true})

    await useAuthStore.getState().logout()

    const s = useAuthStore.getState()
    expect(s.user).toBeNull()
    expect(s.accessToken).toBeNull()
    expect(s.isAuthenticated).toBe(false)
  })

  it('clears the stored refresh token', async () => {
    sessionStorage.setItem('auth_refresh_token', 'ref')

    await useAuthStore.getState().logout()

    expect(sessionStorage.getItem('auth_refresh_token')).toBeNull()
  })

  it('clears state even when the API call fails', async () => {
    mockLogout.mockRejectedValue(new Error('server error'))
    useAuthStore.setState({user: fakeUser, isAuthenticated: true})

    await useAuthStore.getState().logout()

    expect(useAuthStore.getState().isAuthenticated).toBe(false)
  })
})

// ---- refresh ----

describe('refresh', () => {
  it('updates the access token and returns true on success', async () => {
    sessionStorage.setItem('auth_refresh_token', 'refresh-xyz')
    mockRefresh.mockResolvedValue({accessToken: 'new-access', expiresAt: new Date().toISOString()})

    const ok = await useAuthStore.getState().refresh()

    expect(ok).toBe(true)
    expect(useAuthStore.getState().accessToken).toBe('new-access')
  })

  it('clears auth and returns false when no refresh token is stored', async () => {
    useAuthStore.setState({user: fakeUser, isAuthenticated: true})

    const ok = await useAuthStore.getState().refresh()

    expect(ok).toBe(false)
    expect(useAuthStore.getState().isAuthenticated).toBe(false)
  })

  it('clears auth and returns false when refresh API fails', async () => {
    sessionStorage.setItem('auth_refresh_token', 'expired-refresh')
    mockRefresh.mockRejectedValue(new Error('expired'))
    useAuthStore.setState({user: fakeUser, isAuthenticated: true})

    const ok = await useAuthStore.getState().refresh()

    expect(ok).toBe(false)
    expect(useAuthStore.getState().isAuthenticated).toBe(false)
  })
})

// ---- loadFromStorage ----

describe('loadFromStorage', () => {
  it('restores session when a valid (unexpired) refresh token is stored', async () => {
    sessionStorage.setItem('auth_refresh_token', makeJwt())
    mockRefresh.mockResolvedValue({accessToken: 'new-access', expiresAt: new Date().toISOString()})
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

  it('skips all network calls when the stored token is already expired', async () => {
    sessionStorage.setItem('auth_refresh_token', makeJwt(-1000)) // exp in the past

    await useAuthStore.getState().loadFromStorage()

    expect(useAuthStore.getState().isAuthenticated).toBe(false)
    expect(mockRefresh).not.toHaveBeenCalled()
    expect(mockMe).not.toHaveBeenCalled()
    // Stale token is cleared so it won't be retried on the next load.
    expect(sessionStorage.getItem('auth_refresh_token')).toBeNull()
  })

  it('does not call me() when refresh fails', async () => {
    sessionStorage.setItem('auth_refresh_token', makeJwt())
    mockRefresh.mockRejectedValue(new Error('refresh rejected'))

    await useAuthStore.getState().loadFromStorage()

    expect(useAuthStore.getState().isAuthenticated).toBe(false)
    expect(mockMe).not.toHaveBeenCalled()
  })
})

// ---- clearError ----

describe('clearError', () => {
  it('resets the error field', () => {
    useAuthStore.setState({error: 'some error'})
    useAuthStore.getState().clearError()
    expect(useAuthStore.getState().error).toBeNull()
  })
})
