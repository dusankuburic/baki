import {request} from './client'

// A governance alert row, mirroring storageif.GovernanceAlert on the backend.
export interface GovernanceAlert {
  id: string
  flowId: string
  flowName?: string
  orgId?: string
  type: 'drift' | 'health_regression' | 'finding_assigned' | 'comment_added'
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
  /** Personal delivery target (assignment/comment alerts); empty = team-wide. */
  targetUser?: string
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

  markRead: (id: string): Promise<{status: string}> => request('/api/governance/alerts/read', {body: {id}}),

  markAllRead: (): Promise<{status: string}> => request('/api/governance/alerts/read-all', {body: {}}),

  dismiss: (id: string): Promise<{status: string}> => request('/api/governance/alerts/dismiss', {body: {id}}),

  clear: (): Promise<{status: string}> => request('/api/governance/alerts', {method: 'DELETE', body: {}}),
}

// ── Org notification channels (R2-3) ────────────────────────────────────────
// Org admins configure their own webhook/Teams/Slack destinations; governance
// events for the org's flows are delivered there in addition to the
// deployment-global channels.

export type ChannelKind = 'webhook' | 'teams' | 'slack'

export interface OrgChannel {
  id: string
  name: string
  kind: ChannelKind
  url: string
  enabled: boolean
  createdAt: string
}

export interface OrgChannelInput {
  id?: string
  name: string
  kind: ChannelKind
  url: string
  secret?: string
  enabled?: boolean
}

export const channelsApi = {
  list: (orgId: string): Promise<OrgChannel[]> =>
    request(`/api/orgs/${orgId}/channels`, {method: 'GET'}),

  save: (orgId: string, ch: OrgChannelInput): Promise<OrgChannel> =>
    request(`/api/orgs/${orgId}/channels`, {body: ch}),

  remove: (orgId: string, channelId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/channels/${channelId}`, {method: 'DELETE', body: {}}),

  // Synchronous probe: 200 = delivered, non-2xx carries the failure.
  test: (orgId: string, channelId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/channels/${channelId}/test`, {body: {}}),
}
