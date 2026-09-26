import {request} from './client'
import {type AuthUser} from './auth'
import type {PagedResponse} from '@/types'

// Mirrors internal/migration.Result (no JSON tags, so the keys are Go field names).
export interface MigrationResult {
  FlowsMigrated: number
  FlowsSkipped: number
  FlowsFailed: number
  ConversationsMigrated: number
  ConversationsSkipped: number
  ConversationsFailed: number
  SettingsMoved: boolean
  /** Flows whose recorded owner does not exist here, reassigned to the operator. */
  FlowsAdopted: number
  NotMigrated: string[] | null
  Errors: Array<{FlowID: string; Message: string}> | null
  Duration: number
}

// Mirrors api.MigrationStatus. This used to declare a `status` string the
// server never sends, so the panel showed no state, never polled a running
// migration and kept Start enabled where migration is not configured.
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
  /** null when the event carried no metadata (the server marshals a nil map as null). */
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

/** The admin list's role filter: '' is every role, 'none' the accounts without a valid one. */
export type RoleFilter = '' | UserRole | 'none'

// A user as the admin list sees them (storage.User on the Go side). `role` is
// '' when the stored value is not one of the four roles — accounts written
// before the role check existed — and the table shows them as unassigned.
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

// GET /api/admin/users/stats: the whole table counted by role and state, so
// the overview tiles are right however the list is paged or filtered.
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

// SystemHealth mirrors the backend's adminHealthResponse: per-subsystem
// breakdown so an admin sees WHICH dependency is degraded.
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

// Admin reads opt out of the client's GET micro-cache: the page's Refresh
// must reach the server even inside the 5 s window, and a superseded request
// (a filter change, a page turn) is aborted through `signal`.
function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  return request<T>(path, {method: 'GET', noDedup: true, signal})
}

export const adminApi = {
  startMigration: (): Promise<{status: string}> => request('/api/admin/migration/start'),

  getMigrationStatus: (signal?: AbortSignal): Promise<MigrationStatus> => get('/api/admin/migration/status', signal),

  // One page of users plus the number matching the filters: the server pages
  // by limit/offset (max 100) and searches the email and display name.
  listUsers: (query: UserQuery, signal?: AbortSignal): Promise<PagedResponse<AdminUser>> => {
    const params = new URLSearchParams({limit: String(query.limit), offset: String(query.offset)})
    if (query.q) params.set('q', query.q)
    if (query.role) params.set('role', query.role)
    return get(`/api/admin/users/list?${params.toString()}`, signal)
  },

  userStats: (signal?: AbortSignal): Promise<UserStats> => get('/api/admin/users/stats', signal),

  setUserRole: (userId: string, role: string): Promise<void> =>
    request(`/api/admin/users/${userId}/role`, {body: {role}, method: 'PUT'}),

  // ── Operations (R2-6): scanner trigger, per-subsystem health. Instance-admin only.
  triggerScannerScan: (): Promise<{started: boolean}> => request('/api/admin/scanner/scan', {body: {}}),

  systemHealth: (signal?: AbortSignal): Promise<SystemHealth> => get('/api/admin/system/health', signal),

  // The closed set of action names the server emits; the audit filter is
  // built from it so it can never fall behind the backend (it used to offer
  // 7 of 35 actions).
  listAuditActions: (signal?: AbortSignal): Promise<string[]> => get('/api/admin/audit/actions', signal),

  // A bare array, newest first, with no total: the caller asks for one row
  // more than its page to learn whether a next page exists.
  listAuditEvents: (filter: AuditFilter = {}, signal?: AbortSignal): Promise<AuditEvent[]> => {
    const params = new URLSearchParams()
    if (filter.userId) params.set('userId', filter.userId)
    if (filter.action) params.set('action', filter.action)
    if (filter.limit) params.set('limit', String(filter.limit))
    if (filter.offset !== undefined) params.set('offset', String(filter.offset))
    const qs = params.toString()
    return get(`/api/admin/audit${qs ? '?' + qs : ''}`, signal)
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
  // restore reverts the current flow content to a historical version's content
  // (written as a new current version, so it's itself recoverable from history).
  restore: (flowId: string, version: number): Promise<unknown> =>
    request(`/api/library/${flowId}/versions/${version}/restore`, {body: {}}),
  // diff returns the structural diff (FlowDiff) between a historical version and
  // the current flow content — + = added since the snapshot, - = removed.
  diff: (flowId: string, version: number): Promise<unknown> =>
    request(`/api/library/${flowId}/versions/${version}/diff`, {method: 'GET'}),
}
