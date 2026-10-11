import {request, requestValidated} from './client'
import {type AuthUser} from './auth'
import {getAdminUsersPageSchema, getAuditActionsSchema, getAuditEventsSchema, getUserStatsSchema} from './schemas'
import type {PagedResponse} from '@/types'

export interface MigrationResult {
  FlowsMigrated: number
  FlowsSkipped: number
  FlowsFailed: number
  ConversationsMigrated: number
  ConversationsSkipped: number
  ConversationsFailed: number
  SettingsMoved: boolean
  FlowsAdopted: number
  NotMigrated: string[] | null
  Errors: Array<{FlowID: string; Message: string}> | null
  Duration: number
}

export interface MigrationStatus {
  configured: boolean
  running: boolean
  startedAt?: string
  result?: MigrationResult
  error?: string
}

export interface AuditEvent {
  id: string
  userId: string
  email: string
  action: string
  resourceType: string
  resourceId: string
  ip: string
  meta: Record<string, string> | null
  createdAt: string
}

export interface AuditFilter {
  userId?: string
  action?: string
  limit?: number
  offset?: number
}

export type UserRole = AuthUser['role']

export type RoleFilter = '' | UserRole | 'none'

export interface AdminUser {
  id: string
  email: string
  role: UserRole | ''
  emailVerified: boolean
  displayName: string
  avatarUrl: string
  createdAt: string
  updatedAt: string
}

export interface UserQuery {
  q?: string
  role?: RoleFilter
  limit: number
  offset: number
}

export interface UserStats {
  total: number
  byRole: Record<UserRole | 'none', number>
  unverified: number
  locked: number
}

export interface FlowVersion {
  id: string
  flowId: string
  version: number
  comment: string
  createdBy: string
  createdAt: string
  metadata: {
    blockCount: number
    subflowCount: number
  }
}

export interface ComponentStatus {
  status: 'ok' | 'error' | 'skipped'
  error?: string
}

export interface SystemHealth {
  database: ComponentStatus
  blob: ComponentStatus
  redis: ComponentStatus
  overall: 'ok' | 'degraded' | 'down'
}

function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  return request<T>(path, {method: 'GET', noDedup: true, signal})
}

export const adminApi = {
  startMigration: (): Promise<{status: string}> => request('/api/admin/migration/start'),

  getMigrationStatus: (signal?: AbortSignal): Promise<MigrationStatus> => get('/api/admin/migration/status', signal),

  listUsers: async (query: UserQuery, signal?: AbortSignal): Promise<PagedResponse<AdminUser>> => {
    const params = new URLSearchParams({limit: String(query.limit), offset: String(query.offset)})
    if (query.q) params.set('q', query.q)
    if (query.role) params.set('role', query.role)
    return requestValidated(`/api/admin/users/list?${params.toString()}`, await getAdminUsersPageSchema(), {
      method: 'GET',
      noDedup: true,
      signal,
    })
  },

  userStats: async (signal?: AbortSignal): Promise<UserStats> =>
    requestValidated('/api/admin/users/stats', await getUserStatsSchema(), {method: 'GET', noDedup: true, signal}),

  setUserRole: (userId: string, role: string): Promise<void> =>
    request(`/api/admin/users/${userId}/role`, {body: {role}, method: 'PUT'}),

  triggerScannerScan: (): Promise<{started: boolean}> => request('/api/admin/scanner/scan', {body: {}}),

  systemHealth: (signal?: AbortSignal): Promise<SystemHealth> => get('/api/admin/system/health', signal),

  listAuditActions: async (signal?: AbortSignal): Promise<string[]> =>
    requestValidated('/api/admin/audit/actions', await getAuditActionsSchema(), {method: 'GET', noDedup: true, signal}),

  listAuditEvents: async (filter: AuditFilter = {}, signal?: AbortSignal): Promise<AuditEvent[]> => {
    const params = new URLSearchParams()
    if (filter.userId) params.set('userId', filter.userId)
    if (filter.action) params.set('action', filter.action)
    if (filter.limit) params.set('limit', String(filter.limit))
    if (filter.offset !== undefined) params.set('offset', String(filter.offset))
    const qs = params.toString()
    return requestValidated(`/api/admin/audit${qs ? '?' + qs : ''}`, await getAuditEventsSchema(), {
      method: 'GET',
      noDedup: true,
      signal,
    })
  },
}

export const versionsApi = {
  list: (flowId: string, limit?: number): Promise<FlowVersion[]> => {
    const qs = limit ? `?limit=${limit}` : ''
    return request(`/api/library/${flowId}/versions${qs}`, {method: 'GET'})
  },
  save: (flowId: string, comment?: string): Promise<FlowVersion> =>
    request(`/api/library/${flowId}/versions`, {body: {comment: comment ?? ''}}),
  get: (flowId: string, version: number): Promise<FlowVersion & {content: unknown}> =>
    request(`/api/library/${flowId}/versions/${version}`, {method: 'GET'}),
  restore: (flowId: string, version: number): Promise<unknown> =>
    request(`/api/library/${flowId}/versions/${version}/restore`, {body: {}}),
  diff: (flowId: string, version: number): Promise<unknown> =>
    request(`/api/library/${flowId}/versions/${version}/diff`, {method: 'GET'}),
}
