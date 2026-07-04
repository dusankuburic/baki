import { request, requestBlob } from './client'

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
    request('/api/auth/login', credentials),

  register: (credentials: LoginRequest): Promise<LoginResponse> =>
    request('/api/auth/register', credentials),

  logout: (): Promise<void> =>
    request('/api/auth/logout'),

  refresh: (refreshToken: string): Promise<RefreshResponse> =>
    request('/api/auth/refresh', { refreshToken }),

  me: (): Promise<AuthUser> =>
    request('/api/auth/me', undefined, 'GET'),

  updateProfile: (profile: UpdateProfileRequest): Promise<AuthUser> =>
    request('/api/auth/profile', profile, 'PUT'),

  // The backend decodes the JSON key `oldPassword`; map the param to it.
  changePassword: (currentPassword: string, newPassword: string): Promise<void> =>
    request('/api/auth/change-password', { oldPassword: currentPassword, newPassword }),

  // Account recovery / verification (pre-authentication). forgotPassword always
  // resolves for any input — the server never reveals whether an email exists.
  forgotPassword: (email: string): Promise<{ status: string }> =>
    request('/api/auth/forgot-password', { email }),

  resetPassword: (token: string, newPassword: string): Promise<{ status: string }> =>
    request('/api/auth/reset-password', { token, newPassword }),

  verifyEmail: (token: string): Promise<{ status: string }> =>
    request('/api/auth/verify-email', { token }),

  listSessions: (): Promise<SessionInfo[]> =>
    request('/api/auth/sessions', undefined, 'GET'),

  revokeSession: (id: string): Promise<void> =>
    request(`/api/auth/sessions/${id}`, undefined, 'DELETE'),

  ssoInfo: (): Promise<SSOInfo> =>
    request('/api/auth/sso/info', undefined, 'GET'),

  // Exchanges the single-use ticket from the OIDC callback redirect for a
  // regular token pair (same shape as login).
  ssoExchange: (ticket: string): Promise<LoginResponse> =>
    request('/api/auth/sso/exchange', { ticket }),

  // Machine API tokens (PATs) for headless/CI access.
  listApiTokens: (): Promise<ApiToken[]> =>
    request('/api/auth/tokens', undefined, 'GET'),

  // expiresInDays <= 0 (or omitted) creates a non-expiring token. The returned
  // CreatedApiToken.token is the only time the raw secret is available.
  createApiToken: (name: string, expiresInDays?: number): Promise<CreatedApiToken> =>
    request('/api/auth/tokens', { name, expiresInDays: expiresInDays ?? 0 }, 'POST'),

  revokeApiToken: (id: string): Promise<void> =>
    request(`/api/auth/tokens/${id}`, undefined, 'DELETE'),

  // Self-service GDPR data-subject export (downloads a JSON bundle).
  exportAccount: (): Promise<Blob> =>
    requestBlob('/api/auth/account/export', 'GET'),

  // Self-service account erasure. confirmEmail must match the caller's email.
  deleteAccount: (confirmEmail: string): Promise<{ status: string }> =>
    request('/api/auth/account', { confirmEmail }, 'DELETE'),
}
