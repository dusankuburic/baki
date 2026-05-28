import { request } from './client'

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

  changePassword: (currentPassword: string, newPassword: string): Promise<void> =>
    request('/api/auth/change-password', { currentPassword, newPassword }),
}
