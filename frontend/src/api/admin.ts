import { request } from './client'
import { type AuthUser } from './auth'

export interface MigrationResult {
  FlowsMigrated: number
  FlowsSkipped: number
  FlowsFailed: number
  SettingsMoved: boolean
  Errors: Array<{ FlowID: string, Message: string }>
  Duration: number
}

export interface MigrationStatus {
  status: 'idle' | 'running' | 'completed' | 'started'
  result?: MigrationResult
}

export const adminApi = {
  startMigration: (): Promise<{ status: string }> =>
    request('/api/admin/migration/start'),

  getMigrationStatus: (): Promise<MigrationStatus> =>
    request('/api/admin/migration/status', undefined, 'GET'),

  listUsers: (): Promise<AuthUser[]> =>
    request('/api/admin/users/list', undefined, 'GET'),

  setUserRole: (userId: string, role: string): Promise<void> =>
    request(`/api/admin/users/${userId}/role`, { role }),
}
