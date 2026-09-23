import {request} from './client'
import {type AuthUser} from './auth'

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
  meta: Record<string, string>
  createdAt: string
}

export interface AuditFilter {
  userId?: string
  action?: string
  limit?: number
  offset?: number
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

export const adminApi = {
  startMigration: (): Promise<{status: string}> => request('/api/admin/migration/start'),

  getMigrationStatus: (): Promise<MigrationStatus> => request('/api/admin/migration/status', {method: 'GET'}),

  // The endpoint returns a paged envelope {items, total, offset, limit}, not a
  // bare array — handing it to setUsers() verbatim made the dashboard crash
  // on users.filter(). Ask for the server's max page (100) so the dashboard's
  // user list and its "Total Users" stat stay whole for realistic deployments.
  listUsers: async (): Promise<AuthUser[]> => {
    const res = await request<{items: AuthUser[]; total: number}>('/api/admin/users/list?limit=100', {
      method: 'GET',
    })
    return res.items
  },

  setUserRole: (userId: string, role: string): Promise<void> =>
    request(`/api/admin/users/${userId}/role`, {body: {role}, method: 'PUT'}),

  // ── Operations (R2-6): scanner trigger, per-subsystem health. Instance-admin only.
  triggerScannerScan: (): Promise<{started: boolean}> => request('/api/admin/scanner/scan', {body: {}}),

  systemHealth: (): Promise<SystemHealth> => request('/api/admin/system/health', {method: 'GET'}),

  // The closed set of action names the server emits; the audit filter is
  // built from it so it can never fall behind the backend (it used to offer
  // 7 of 35 actions).
  listAuditActions: (): Promise<string[]> => request('/api/admin/audit/actions', {method: 'GET'}),

  listAuditEvents: (filter: AuditFilter = {}): Promise<AuditEvent[]> => {
    const params = new URLSearchParams()
    if (filter.userId) params.set('userId', filter.userId)
    if (filter.action) params.set('action', filter.action)
    if (filter.limit) params.set('limit', String(filter.limit))
    if (filter.offset) params.set('offset', String(filter.offset))
    const qs = params.toString()
    return request(`/api/admin/audit${qs ? '?' + qs : ''}`, {method: 'GET'})
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
