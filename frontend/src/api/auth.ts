import {request, requestValidated, requestBlob, requestForm} from './client'
import {getAuthUserSchema, getLoginResponseSchema, getRegisterResponseSchema, getRefreshResponseSchema} from './schemas'

export interface LoginRequest {
  email: string
  password: string
  rememberMe?: boolean
}

export interface LoginResponse {
  accessToken: string
  refreshToken: string
  expiresAt: string
  user: AuthUser
}

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
  emailVerified?: boolean
  profile?: {
    bio?: string
    jobTitle?: string
    timezone?: string
  }
}

export interface RefreshResponse {
  accessToken: string
  expiresAt: string
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

export interface IdentityLinkInfo {
  provider: string
  email?: string
  createdAt: string
}

export type ApiTokenScope = 'read' | 'write' | 'chat' | 'admin' | 'ci'
export const API_TOKEN_SCOPES: readonly ApiTokenScope[] = ['read', 'write', 'chat', 'admin', 'ci']

export interface ApiToken {
  id: string
  name: string
  createdAt: string
  expiresAt?: string
  scopes?: ApiTokenScope[]
}

export interface CreatedApiToken extends ApiToken {
  token: string
}

export const authApi = {
  login: async (credentials: LoginRequest): Promise<LoginResponse> =>
    requestValidated('/api/auth/login', await getLoginResponseSchema(), {body: credentials}),

  register: async (credentials: LoginRequest): Promise<RegisterResponse> =>
    requestValidated('/api/auth/register', await getRegisterResponseSchema(), {body: credentials}),

  resendVerification: (email: string): Promise<{status: string}> =>
    request('/api/auth/resend-verification', {body: {email}}),

  logout: (): Promise<void> => request('/api/auth/logout'),

  refresh: async (refreshToken: string | null, rememberMe: boolean): Promise<RefreshResponse> =>
    requestValidated(
      '/api/auth/refresh',
      await getRefreshResponseSchema(),
      {body: refreshToken ? {refreshToken, rememberMe} : {rememberMe}},
    ),

  me: async (): Promise<AuthUser> => requestValidated('/api/auth/me', await getAuthUserSchema(), {method: 'GET'}),

  updateProfile: (profile: UpdateProfileRequest): Promise<AuthUser> =>
    request('/api/auth/profile', {body: profile, method: 'PUT'}),

  // The multipart upload and its removal; the server re-encodes the image, so the file never reaches storage
  // as uploaded (metadata stripped, downscaled, size-capped).
  uploadAvatar: (file: File | Blob): Promise<AuthUser> =>
    requestForm('/api/auth/profile/avatar', file),

  deleteAvatar: (): Promise<AuthUser> => request('/api/auth/profile/avatar', {method: 'DELETE'}),

  changePassword: (currentPassword: string, newPassword: string): Promise<void> =>
    request('/api/auth/change-password', {body: {oldPassword: currentPassword, newPassword}}),

  forgotPassword: (email: string): Promise<{status: string}> => request('/api/auth/forgot-password', {body: {email}}),

  resetPassword: (token: string, newPassword: string): Promise<{status: string}> =>
    request('/api/auth/reset-password', {body: {token, newPassword}}),

  verifyEmail: (token: string): Promise<{status: string}> => request('/api/auth/verify-email', {body: {token}}),

  listSessions: (): Promise<SessionInfo[]> => request('/api/auth/sessions', {method: 'GET'}),

  revokeSession: (id: string): Promise<void> => request(`/api/auth/sessions/${id}`, {method: 'DELETE'}),

  ssoInfo: (): Promise<SSOInfo> => request('/api/auth/sso/info', {method: 'GET'}),

  ssoLinkStart: (): Promise<{url: string}> => request('/api/auth/sso/link/start'),
  ssoLinks: (): Promise<IdentityLinkInfo[]> => request('/api/auth/sso/links', {method: 'GET'}),
  ssoUnlink: (provider: string): Promise<void> =>
    request(`/api/auth/sso/links/${encodeURIComponent(provider)}`, {method: 'DELETE'}),

  ssoExchange: async (ticket: string): Promise<LoginResponse> =>
    requestValidated('/api/auth/sso/exchange', await getLoginResponseSchema(), {body: {ticket}}),

  listApiTokens: (): Promise<ApiToken[]> => request('/api/auth/tokens', {method: 'GET'}),

  createApiToken: (name: string, expiresInDays?: number, scopes?: ApiTokenScope[]): Promise<CreatedApiToken> =>
    request('/api/auth/tokens', {body: {name, expiresInDays: expiresInDays ?? 0, scopes: scopes ?? []}}),

  revokeApiToken: (id: string): Promise<void> => request(`/api/auth/tokens/${id}`, {method: 'DELETE'}),

  exportAccount: (): Promise<Blob> => requestBlob('/api/auth/account/export'),

  deleteAccount: (confirmEmail: string): Promise<{status: string}> =>
    request('/api/auth/account', {body: {confirmEmail}, method: 'DELETE'}),
}
