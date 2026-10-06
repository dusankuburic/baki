import {describe, it, expect, vi, beforeEach} from 'vitest'
import {useAuthStore, getCurrentSessionId} from './authStore'
import type {AuthUser, LoginResponse} from '@/api/auth'

vi.mock('@/api/auth', () => ({
  authApi: {
    login: vi.fn(),
    register: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
    me: vi.fn(),
  },
}))

// Web vs desktop auth flows are gated on isTauri(); tests default to web
// (jsdom) and flip the mock for the desktop-semantics suite.
const isTauriMock = vi.fn(() => false)
vi.mock('@/platform/guards', () => ({isTauri: () => isTauriMock()}))

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
// `exp` (seconds) plus any extra claims. Desktop loadFromStorage decodes `exp`
// to decide whether to skip a doomed refresh call. Default: 15 minutes ahead.
function makeJwt(expMsFromNow = 900_000, extra: Record<string, unknown> = {}): string {
  const payload = btoa(JSON.stringify({exp: Math.floor((Date.now() + expMsFromNow) / 1000), ...extra}))
  return `header.${payload}.sig`
}

const fakeLoginResponse: LoginResponse = {
  accessToken: 'access-abc',
  refreshToken: 'refresh-xyz',
  expiresAt: new Date(Date.now() + 900_000).toISOString(),
  user: fakeUser,
}

// jsdom's storage implementations in vitest 4 are incomplete — stub them.
// WEB mode stores no refresh token at all (the HttpOnly cookie is the
// carrier): only the non-sensitive `auth_session_active` marker lands in
// localStorage. DESKTOP mode keeps the refresh token in sessionStorage /
// localStorage (remember me) exactly as before.
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
  isTauriMock.mockReturnValue(false)
  _local = {}
  _session = {}
  vi.resetAllMocks()
  mockLogout.mockResolvedValue(undefined)
})

// ---- register ----

describe('register', () => {
  const mockRegister = authApi.register as ReturnType<typeof vi.fn>

  it('returns verification-required and stays signed out when the server issues no session', async () => {
    // The ordinary sign-up response: account created, verification link sent,
    // no tokens. Treating this as a login used to throw a schema error and
    // report "Registration failed" for an account that had just been created.
    mockRegister.mockResolvedValue({status: 'ok', user: fakeUser, verificationRequired: true})

    const outcome = await useAuthStore.getState().register({email: 'alice@example.com', password: 'secret'})

    expect(outcome).toBe('verification-required')
    const s = useAuthStore.getState()
    expect(s.isAuthenticated).toBe(false)
    expect(s.accessToken).toBeNull()
    expect(s.error).toBeNull()
    expect(s.isLoading).toBe(false)
    // Web: no refresh token may land in JS-readable storage.
    expect(_session['auth_refresh_token']).toBeUndefined()
    expect(_local['auth_refresh_token']).toBeUndefined()
  })

  it('sends rememberMe on the wire and signs in directly when the server returns a session (bootstrap admin)', async () => {
    mockRegister.mockResolvedValue({...fakeLoginResponse, status: 'ok', verificationRequired: false})

    const outcome = await useAuthStore.getState().register({email: 'alice@example.com', password: 'secret'}, true)

    expect(outcome).toBe('signed-in')
    expect(mockRegister).toHaveBeenCalledWith({email: 'alice@example.com', password: 'secret', rememberMe: true})
    const s = useAuthStore.getState()
    expect(s.isAuthenticated).toBe(true)
    expect(s.user).toEqual(fakeUser)
    expect(s.accessToken).toBe('access-abc')
    // Web: persistent marker, no token in storage.
    expect(_local['auth_session_active']).toBe('p')
    expect(_local['auth_refresh_token']).toBeUndefined()
  })

  it('surfaces the error message and rethrows on failure', async () => {
    mockRegister.mockRejectedValue(new Error('email already registered'))

    await expect(useAuthStore.getState().register({email: 'alice@example.com', password: 'secret'})).rejects.toThrow(
      'email already registered',
    )
    expect(useAuthStore.getState().error).toBe('email already registered')
    expect(useAuthStore.getState().isAuthenticated).toBe(false)
  })
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

  it('web: never stores the refresh token — the marker records the session and remember-me', async () => {
    mockLogin.mockResolvedValue(fakeLoginResponse)
    await useAuthStore.getState().login({email: 'a@b.com', password: 'p'})

    // Access token lives in memory/state only; the refresh token never lands
    // in JS-readable storage (the HttpOnly cookie is the carrier).
    expect(useAuthStore.getState().accessToken).toBe('access-abc')
    expect(localStorage.getItem('auth_access_token')).toBeNull()
    expect(localStorage.getItem('auth_refresh_token')).toBeNull()
    expect(sessionStorage.getItem('auth_refresh_token')).toBeNull()
    expect(localStorage.getItem('auth_session_active')).toBe('1')

    // The wire body carries rememberMe so the backend shapes the cookie.
    expect(mockLogin).toHaveBeenCalledWith({email: 'a@b.com', password: 'p', rememberMe: false})
  })

  it('web: remember-me writes the persistent marker', async () => {
    mockLogin.mockResolvedValue(fakeLoginResponse)
    await useAuthStore.getState().login({email: 'a@b.com', password: 'p'}, true)

    expect(localStorage.getItem('auth_session_active')).toBe('p')
    expect(mockLogin).toHaveBeenCalledWith({email: 'a@b.com', password: 'p', rememberMe: true})
  })

  it('desktop: stores the refresh token in sessionStorage and the access token in state', async () => {
    isTauriMock.mockReturnValue(true)
    mockLogin.mockResolvedValue(fakeLoginResponse)
    await useAuthStore.getState().login({email: 'a@b.com', password: 'p'})

    expect(useAuthStore.getState().accessToken).toBe('access-abc')
    expect(sessionStorage.getItem('auth_refresh_token')).toBe('refresh-xyz')
    expect(localStorage.getItem('auth_session_active')).toBeNull()
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

  it('web: clears the session marker', async () => {
    localStorage.setItem('auth_session_active', 'p')

    await useAuthStore.getState().logout()

    expect(localStorage.getItem('auth_session_active')).toBeNull()
  })

  it('desktop: clears the stored refresh token', async () => {
    isTauriMock.mockReturnValue(true)
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

  // Regression: logout must ask the service worker to drop its caches so a
  // shared device doesn't retain session artifacts in Cache Storage.
  it('tells the service worker to purge caches', async () => {
    const postMessage = vi.fn()
    vi.stubGlobal('navigator', {serviceWorker: {controller: {postMessage}}})
    useAuthStore.setState({user: fakeUser, accessToken: 'tok', isAuthenticated: true})

    await useAuthStore.getState().logout()

    expect(postMessage).toHaveBeenCalledWith({type: 'PURGE_CACHES'})
    // NOTE: no vi.unstubAllGlobals() here — it would uninstall the
    // localStorage/sessionStorage stubs for every later test in this file and
    // leak writes into jsdom's real storage. The navigator stub is harmless.
  })

  it('logout still succeeds with no service worker registered', async () => {
    vi.stubGlobal('navigator', {serviceWorker: {controller: null}})
    useAuthStore.setState({user: fakeUser, accessToken: 'tok', isAuthenticated: true})

    await useAuthStore.getState().logout()

    expect(useAuthStore.getState().isAuthenticated).toBe(false)
  })
})

// ---- refresh ----

describe('refresh (web: cookie-authenticated)', () => {
  it('rotates via the cookie — null token body, marker-shaped rememberMe', async () => {
    localStorage.setItem('auth_session_active', 'p')
    mockRefresh.mockResolvedValue({accessToken: 'new-access', expiresAt: new Date().toISOString()})

    const ok = await useAuthStore.getState().refresh()

    expect(ok).toBe(true)
    expect(mockRefresh).toHaveBeenCalledWith(null, true)
    expect(useAuthStore.getState().accessToken).toBe('new-access')
  })

  it('captures the rotated token jti in memory for the sessions UI', async () => {
    localStorage.setItem('auth_session_active', '1')
    mockRefresh.mockResolvedValue({
      accessToken: 'new-access',
      refreshToken: makeJwt(900_000, {jti: 'jti-123'}),
      expiresAt: new Date().toISOString(),
    })

    await useAuthStore.getState().refresh()

    expect(getCurrentSessionId()).toBe('jti-123')
  })

  it('clears auth and returns false when no session marker exists (no network call)', async () => {
    useAuthStore.setState({user: fakeUser, isAuthenticated: true})

    const ok = await useAuthStore.getState().refresh()

    expect(ok).toBe(false)
    expect(useAuthStore.getState().isAuthenticated).toBe(false)
    expect(mockRefresh).not.toHaveBeenCalled()
  })

  it('clears auth and the marker when refresh fails', async () => {
    localStorage.setItem('auth_session_active', '1')
    mockRefresh.mockRejectedValue(new Error('expired'))
    useAuthStore.setState({user: fakeUser, isAuthenticated: true})

    const ok = await useAuthStore.getState().refresh()

    expect(ok).toBe(false)
    expect(useAuthStore.getState().isAuthenticated).toBe(false)
    expect(localStorage.getItem('auth_session_active')).toBeNull()
  })
})

describe('refresh (desktop: body token)', () => {
  beforeEach(() => {
    isTauriMock.mockReturnValue(true)
  })

  it('sends the stored token and updates the access token', async () => {
    sessionStorage.setItem('auth_refresh_token', 'refresh-xyz')
    mockRefresh.mockResolvedValue({accessToken: 'new-access', expiresAt: new Date().toISOString()})

    const ok = await useAuthStore.getState().refresh()

    expect(ok).toBe(true)
    expect(mockRefresh).toHaveBeenCalledWith('refresh-xyz', false)
    expect(useAuthStore.getState().accessToken).toBe('new-access')
  })

  it('rotates the stored token when the server issues one', async () => {
    sessionStorage.setItem('auth_refresh_token', 'refresh-xyz')
    mockRefresh.mockResolvedValue({
      accessToken: 'new-access',
      refreshToken: 'refresh-rotated',
      expiresAt: new Date().toISOString(),
    })

    const ok = await useAuthStore.getState().refresh()

    expect(ok).toBe(true)
    expect(sessionStorage.getItem('auth_refresh_token')).toBe('refresh-rotated')
  })

  it('clears auth and returns false when no refresh token is stored', async () => {
    useAuthStore.setState({user: fakeUser, isAuthenticated: true})

    const ok = await useAuthStore.getState().refresh()

    expect(ok).toBe(false)
    expect(useAuthStore.getState().isAuthenticated).toBe(false)
    expect(mockRefresh).not.toHaveBeenCalled()
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

describe('loadFromStorage (web)', () => {
  it('restores the session from the marker via a cookie refresh', async () => {
    localStorage.setItem('auth_session_active', '1')
    mockRefresh.mockResolvedValue({accessToken: 'new-access', expiresAt: new Date().toISOString()})
    mockMe.mockResolvedValue(fakeUser)

    await useAuthStore.getState().loadFromStorage()

    expect(useAuthStore.getState().isAuthenticated).toBe(true)
    expect(useAuthStore.getState().user).toEqual(fakeUser)
  })

  it('does nothing when no marker and no legacy token exist', async () => {
    await useAuthStore.getState().loadFromStorage()
    expect(useAuthStore.getState().isAuthenticated).toBe(false)
    expect(mockRefresh).not.toHaveBeenCalled()
    expect(mockMe).not.toHaveBeenCalled()
  })

  it('migrates a legacy stored token: marker written, storage cleared, session restored', async () => {
    sessionStorage.setItem('auth_refresh_token', makeJwt())
    mockRefresh.mockResolvedValue({accessToken: 'new-access', expiresAt: new Date().toISOString()})
    mockMe.mockResolvedValue(fakeUser)

    await useAuthStore.getState().loadFromStorage()

    expect(useAuthStore.getState().isAuthenticated).toBe(true)
    expect(localStorage.getItem('auth_session_active')).toBe('1')
    expect(sessionStorage.getItem('auth_refresh_token')).toBeNull()
  })

  it('does not call me() when refresh fails', async () => {
    localStorage.setItem('auth_session_active', '1')
    mockRefresh.mockRejectedValue(new Error('refresh rejected'))

    await useAuthStore.getState().loadFromStorage()

    expect(useAuthStore.getState().isAuthenticated).toBe(false)
    expect(mockMe).not.toHaveBeenCalled()
  })
})

describe('loadFromStorage (desktop)', () => {
  beforeEach(() => {
    isTauriMock.mockReturnValue(true)
  })

  it('restores session when a valid (unexpired) refresh token is stored', async () => {
    sessionStorage.setItem('auth_refresh_token', makeJwt())
    mockRefresh.mockResolvedValue({accessToken: 'new-access', expiresAt: new Date().toISOString()})
    mockMe.mockResolvedValue(fakeUser)

    await useAuthStore.getState().loadFromStorage()

    expect(useAuthStore.getState().isAuthenticated).toBe(true)
    expect(useAuthStore.getState().user).toEqual(fakeUser)
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
})

// ---- clearError ----

describe('clearError', () => {
  it('resets the error field', () => {
    useAuthStore.setState({error: 'some error'})
    useAuthStore.getState().clearError()
    expect(useAuthStore.getState().error).toBeNull()
  })
})
