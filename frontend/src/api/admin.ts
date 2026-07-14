import {request} from './client'
import {type AuthUser} from './auth'

export interface MigrationResult {
  FlowsMigrated: number
  FlowsSkipped: number
  FlowsFailed: number
  SettingsMoved: boolean
  Errors: Array<{FlowID: string; Message: string}>
  Duration: number
}

export interface MigrationStatus {
  status: 'idle' | 'running' | 'completed' | 'started'
  result?: MigrationResult
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

export const adminApi = {
  startMigration: (): Promise<{status: string}> => request('/api/admin/migration/start'),

  getMigrationStatus: (): Promise<MigrationStatus> => request('/api/admin/migration/status', undefined, 'GET'),

  listUsers: (): Promise<AuthUser[]> => request('/api/admin/users/list', undefined, 'GET'),

  setUserRole: (userId: string, role: string): Promise<void> =>
    request(`/api/admin/users/${userId}/role`, {role}, 'PUT'),

  listAuditEvents: (filter: AuditFilter = {}): Promise<AuditEvent[]> => {
    const params = new URLSearchParams()
    if (filter.userId) params.set('userId', filter.userId)
    if (filter.action) params.set('action', filter.action)
    if (filter.limit) params.set('limit', String(filter.limit))
    if (filter.offset) params.set('offset', String(filter.offset))
    const qs = params.toString()
    return request(`/api/admin/audit${qs ? '?' + qs : ''}`, undefined, 'GET')
  },
}

export const versionsApi = {
  list: (flowId: string, limit?: number): Promise<FlowVersion[]> => {
    const qs = limit ? `?limit=${limit}` : ''
    return request(`/api/library/${flowId}/versions${qs}`, undefined, 'GET')
  },
  save: (flowId: string, comment?: string): Promise<FlowVersion> =>
    request(`/api/library/${flowId}/versions`, {comment: comment ?? ''}),
  get: (flowId: string, version: number): Promise<FlowVersion & {content: unknown}> =>
    request(`/api/library/${flowId}/versions/${version}`, undefined, 'GET'),
  // restore reverts the current flow content to a historical version's content
  // (written as a new current version, so it's itself recoverable from history).
  restore: (flowId: string, version: number): Promise<unknown> =>
    request(`/api/library/${flowId}/versions/${version}/restore`, {}),
  // diff returns the structural diff (FlowDiff) between a historical version and
  // the current flow content — + = added since the snapshot, - = removed.
  diff: (flowId: string, version: number): Promise<unknown> =>
    request(`/api/library/${flowId}/versions/${version}/diff`, undefined, 'GET'),
}
