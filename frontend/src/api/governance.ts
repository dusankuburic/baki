import {request} from './client'

// A governance alert row, mirroring storageif.GovernanceAlert on the backend.
export interface GovernanceAlert {
  id: string
  flowId: string
  flowName?: string
  orgId?: string
  type: 'drift' | 'health_regression'
  title: string
  message?: string
  severity: 'error' | 'warning'
  newErrors?: number
  newWarnings?: number
  healthScore?: number
  prevHealth?: number
  createdAt: string
  readAt?: string | null
  dismissedAt?: string | null
}

export interface GovernanceAlertFilter {
  limit?: number
  offset?: number
  includeDismissed?: boolean
}

export const governanceApi = {
  list: (filter: GovernanceAlertFilter = {}): Promise<GovernanceAlert[]> => {
    const params = new URLSearchParams()
    if (filter.limit != null) params.set('limit', String(filter.limit))
    if (filter.offset != null) params.set('offset', String(filter.offset))
    if (filter.includeDismissed) params.set('includeDismissed', 'true')
    const qs = params.toString()
    return request(`/api/governance/alerts${qs ? '?' + qs : ''}`, {method: 'GET'})
  },

  unreadCount: (): Promise<{count: number}> => request('/api/governance/alerts/unread-count', {method: 'GET'}),

  markRead: (id: string): Promise<{status: string}> =>
    request('/api/governance/alerts/read', {body: {id}}),

  markAllRead: (): Promise<{status: string}> =>
    request('/api/governance/alerts/read-all', {body: {}}),

  dismiss: (id: string): Promise<{status: string}> =>
    request('/api/governance/alerts/dismiss', {body: {id}}),

  clear: (): Promise<{status: string}> =>
    request('/api/governance/alerts', {method: 'DELETE', body: {}}),
}
