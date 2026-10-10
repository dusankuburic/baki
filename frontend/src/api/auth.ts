import {request, requestValidated, requestBlob} from './client'
import {getAuthUserSchema, getLoginResponseSchema, getRegisterResponseSchema, getRefreshResponseSchema} from './schemas'

export interface LoginRequest {
  email: string
  password: string
  // Shapes the backend's HttpOnly refresh cookie: persistent (Expires =
  // refresh-token TTL) vs a browser-session cookie. Desktop ignores it —
  // it authenticates with the body token and never relies on the cookie.
  rememberMe?: boolean
}

export interface LoginResponse {
  accessToken: string
  refreshToken: string
  expiresAt: string
  user: AuthUser
}

// RegisterResponse: `verificationRequired` is the discriminator. When true the
// account exists but has no session yet — the user must follow the emailed
// link. When false (bootstrap admin) the token fields are present and the
// response doubles as a login.
export interface RegisterResponse {
  status?: string
  user: AuthUser
  verificationRequired: boolean
  accessToken?: string
  refreshToken?: string
  expiresAt?: string
}

export interface AuthUser {
  id: string
  email: string
  role: 'admin' | 'member' | 'viewer' | 'guest'
  displayName?: string
  avatarUrl?: string
  createdAt?: string
}

export interface RefreshResponse {
  accessToken: string
  expiresAt: string
  // Present when the server rotates refresh tokens; the client should persist it.
  refreshToken?: string
}

export interface UpdateProfileRequest {
  displayName: string
  avatarUrl: string
}

export interface SessionInfo {
  id: string
  createdAt: string
  expiresAt: string
  userAgent?: string
  ip?: string
}

export interface SSOInfo {
  enabled: boolean
  provider?: string
}

// IdentityLinkInfo is one IdP identity attached to the current account (the
// opaque IdP subject stays server-side).
export interface IdentityLinkInfo {
  provider: string
  email?: string
  createdAt: string
}

// ApiTokenScope mirrors the closed scope set in internal/auth/apitoken.go.
// A token with NO scopes is unscoped: it can do everything its owner can,
// except call the CI webhook, which takes the ci scope only.
export type ApiTokenScope = 'read' | 'write' | 'chat' | 'admin' | 'ci'
export const API_TOKEN_SCOPES: readonly ApiTokenScope[] = ['read', 'write', 'chat', 'admin', 'ci']

// ApiToken is the metadata for a machine token (the secret is never returned).
export interface ApiToken {
  id: string
  name: string
  createdAt: string
  expiresAt?: string
  // Empty/omitted = unscoped (full access).
  scopes?: ApiTokenScope[]
}

// CreatedApiToken additionally carries the raw token, shown exactly once.
export interface CreatedApiToken extends ApiToken {
  token: string
}

export const authApi = {
  login: async (credentials: LoginRequest): Promise<LoginResponse> =>
    requestValidated('/api/auth/login', await getLoginResponseSchema(), {body: credentials}),

  register: async (credentials: LoginRequest): Promise<RegisterResponse> =>
    requestValidated('/api/auth/register', await getRegisterResponseSchema(), {body: credentials}),

  // Re-sends the verification link. Like forgotPassword it always resolves —
  // the server answers identically for unknown, verified and pending addresses.
  resendVerification: (email: string): Promise<{status: string}> =>
    request('/api/auth/resend-verification', {body: {email}}),

  logout: (): Promise<void> => request('/api/auth/logout'),

  // Web clients pass refreshToken: null — the HttpOnly `pad_refresh` cookie
  // (scoped to /api/auth) authenticates the rotation and the server rotates
  // the cookie in place; the backend's body-token fallback keeps the desktop
  // build working unchanged. rememberMe reshapes the rotated cookie.
  refresh: async (refreshToken: string | null, rememberMe: boolean): Promise<RefreshResponse> =>
    requestValidated(
      '/api/auth/refresh',
      await getRefreshResponseSchema(),
      {body: refreshToken ? {refreshToken, rememberMe} : {rememberMe}},
    ),

  me: async (): Promise<AuthUser> => requestValidated('/api/auth/me', await getAuthUserSchema(), {method: 'GET'}),

  updateProfile: (profile: UpdateProfileRequest): Promise<AuthUser> =>
    request('/api/auth/profile', {body: profile, method: 'PUT'}),

  // The backend decodes the JSON key `oldPassword`; map the param to it.
  changePassword: (currentPassword: string, newPassword: string): Promise<void> =>
    request('/api/auth/change-password', {body: {oldPassword: currentPassword, newPassword}}),

  // Account recovery / verification (pre-authentication). forgotPassword always
  // resolves for any input — the server never reveals whether an email exists.
  forgotPassword: (email: string): Promise<{status: string}> => request('/api/auth/forgot-password', {body: {email}}),

  resetPassword: (token: string, newPassword: string): Promise<{status: string}> =>
    request('/api/auth/reset-password', {body: {token, newPassword}}),

  verifyEmail: (token: string): Promise<{status: string}> => request('/api/auth/verify-email', {body: {token}}),

  listSessions: (): Promise<SessionInfo[]> => request('/api/auth/sessions', {method: 'GET'}),

  revokeSession: (id: string): Promise<void> => request(`/api/auth/sessions/${id}`, {method: 'DELETE'}),

  ssoInfo: (): Promise<SSOInfo> => request('/api/auth/sso/info', {method: 'GET'}),

  // Account linking: start a link round trip as an authenticated XHR (a
  // top-level navigation cannot carry the bearer token), then navigate the
  // browser to the returned IdP URL; the callback lands on /#ssoLinked=<provider>.
  ssoLinkStart: (): Promise<{url: string}> => request('/api/auth/sso/link/start'),
  ssoLinks: (): Promise<IdentityLinkInfo[]> => request('/api/auth/sso/links', {method: 'GET'}),
  ssoUnlink: (provider: string): Promise<void> =>
    request(`/api/auth/sso/links/${encodeURIComponent(provider)}`, {method: 'DELETE'}),

  // Exchanges the single-use ticket from the OIDC callback redirect for a
  // regular token pair (same shape as login).
  ssoExchange: async (ticket: string): Promise<LoginResponse> =>
    requestValidated('/api/auth/sso/exchange', await getLoginResponseSchema(), {body: {ticket}}),

  // Machine API tokens (PATs) for headless/CI access.
  listApiTokens: (): Promise<ApiToken[]> => request('/api/auth/tokens', {method: 'GET'}),

  // expiresInDays <= 0 (or omitted) creates a non-expiring token. The returned
  // CreatedApiToken.token is the only time the raw secret is available.
  // scopes: subset of ApiTokenScope; empty/omitted mints an UNSCOPED token that
  // can do everything its owner can (see internal/auth/apitoken.go).
  createApiToken: (name: string, expiresInDays?: number, scopes?: ApiTokenScope[]): Promise<CreatedApiToken> =>
    request('/api/auth/tokens', {body: {name, expiresInDays: expiresInDays ?? 0, scopes: scopes ?? []}}),

  revokeApiToken: (id: string): Promise<void> => request(`/api/auth/tokens/${id}`, {method: 'DELETE'}),

  // Self-service GDPR data-subject export (downloads a JSON bundle).
  exportAccount: (): Promise<Blob> => requestBlob('/api/auth/account/export'),

  // Self-service account erasure. confirmEmail must match the caller's email.
  deleteAccount: (confirmEmail: string): Promise<{status: string}> =>
    request('/api/auth/account', {body: {confirmEmail}, method: 'DELETE'}),
}
