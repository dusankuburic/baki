import {request, requestValidated, requestBlob} from './client'
import {LoginResponseSchema, RefreshResponseSchema, AuthUserSchema} from './schemas'

export interface LoginRequest {
  email: string
  password: string
}

export interface LoginResponse {
  accessToken: string
  refreshToken: string
  expiresAt: string
  user: AuthUser
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

// ApiToken is the metadata for a machine token (the secret is never returned).
export interface ApiToken {
  id: string
  name: string
  createdAt: string
  expiresAt?: string
}

// CreatedApiToken additionally carries the raw token, shown exactly once.
export interface CreatedApiToken extends ApiToken {
  token: string
}

export const authApi = {
  login: (credentials: LoginRequest): Promise<LoginResponse> =>
    requestValidated('/api/auth/login', LoginResponseSchema, {body: credentials}),

  register: (credentials: LoginRequest): Promise<LoginResponse> =>
    requestValidated('/api/auth/register', LoginResponseSchema, {body: credentials}),

  logout: (): Promise<void> => request('/api/auth/logout'),

  refresh: (refreshToken: string): Promise<RefreshResponse> =>
    requestValidated('/api/auth/refresh', RefreshResponseSchema, {body: {refreshToken}}),

  me: (): Promise<AuthUser> => requestValidated('/api/auth/me', AuthUserSchema, {method: 'GET'}),

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

  // Exchanges the single-use ticket from the OIDC callback redirect for a
  // regular token pair (same shape as login).
  ssoExchange: (ticket: string): Promise<LoginResponse> =>
    requestValidated('/api/auth/sso/exchange', LoginResponseSchema, {body: {ticket}}),

  // Machine API tokens (PATs) for headless/CI access.
  listApiTokens: (): Promise<ApiToken[]> => request('/api/auth/tokens', {method: 'GET'}),

  // expiresInDays <= 0 (or omitted) creates a non-expiring token. The returned
  // CreatedApiToken.token is the only time the raw secret is available.
  createApiToken: (name: string, expiresInDays?: number): Promise<CreatedApiToken> =>
    request('/api/auth/tokens', {body: {name, expiresInDays: expiresInDays ?? 0}}),

  revokeApiToken: (id: string): Promise<void> => request(`/api/auth/tokens/${id}`, {method: 'DELETE'}),

  // Self-service GDPR data-subject export (downloads a JSON bundle).
  exportAccount: (): Promise<Blob> => requestBlob('/api/auth/account/export'),

  // Self-service account erasure. confirmEmail must match the caller's email.
  deleteAccount: (confirmEmail: string): Promise<{status: string}> =>
    request('/api/auth/account', {body: {confirmEmail}, method: 'DELETE'}),
}
