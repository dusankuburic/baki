import {ApiError, request, requestValidated} from './client'
import {
  getChannelOptionsSchema,
  getCreatedOrgCICredentialSchema,
  getGovernanceAlertsSchema,
  getOrgCICredentialsSchema,
  getOrgChannelsSchema,
  getUnreadCountSchema,
} from './schemas'
import type {Tolerant} from '@/types'

export type GovernanceAlertType = 'drift' | 'health_regression' | 'finding_assigned' | 'comment_added' | 'new_rules'
export type GovernanceNoticeScope = 'org' | 'user' | 'app'
export type GovernanceAlertSeverity = 'error' | 'warning'

export interface GovernanceAlert {
  id: string
  flowId: string
  flowName?: string
  orgId?: string
  type: Tolerant<GovernanceAlertType>
  title: string
  message?: string
  severity: Tolerant<GovernanceAlertSeverity>
  newErrors?: number
  newWarnings?: number
  healthScore?: number
  prevHealth?: number
  createdAt: string
  readAt?: string | null
  dismissedAt?: string | null
  targetUser?: string
  ruleIds?: string[]
  scope?: Tolerant<GovernanceNoticeScope>
  flowCount?: number
}

export interface GovernanceAlertFilter {
  limit?: number
  offset?: number
  includeDismissed?: boolean
}

export const governanceApi = {
  list: async (filter: GovernanceAlertFilter = {}): Promise<GovernanceAlert[]> => {
    const params = new URLSearchParams()
    if (filter.limit != null) params.set('limit', String(filter.limit))
    if (filter.offset != null) params.set('offset', String(filter.offset))
    if (filter.includeDismissed) params.set('includeDismissed', 'true')
    const qs = params.toString()
    return requestValidated(`/api/governance/alerts${qs ? '?' + qs : ''}`, await getGovernanceAlertsSchema(), {
      method: 'GET',
    })
  },

  unreadCount: async (): Promise<{count: number}> =>
    requestValidated('/api/governance/alerts/unread-count', await getUnreadCountSchema(), {method: 'GET'}),

  markAllRead: (): Promise<{status: string}> => request('/api/governance/alerts/read-all', {body: {}}),

  dismiss: (id: string): Promise<{status: string}> => request('/api/governance/alerts/dismiss', {body: {id}}),

  clear: (): Promise<{status: string}> => request('/api/governance/alerts', {method: 'DELETE', body: {}}),
}

export const CHANNEL_KINDS = ['webhook', 'teams', 'teams-workflow', 'slack', 'jira', 'email'] as const
export type ChannelKind = (typeof CHANNEL_KINDS)[number]

export const LEGACY_CHANNEL_KINDS: readonly ChannelKind[] = ['webhook', 'teams', 'teams-workflow', 'slack']

export const SIGNED_CHANNEL_KINDS: readonly ChannelKind[] = ['webhook', 'slack']

export const channelSigns = (kind: ChannelKind): boolean => SIGNED_CHANNEL_KINDS.includes(kind)

export const channelHasUrl = (kind: ChannelKind): boolean => kind !== 'email'

export const CHANNEL_EVENTS = ['drift', 'health_regression', 'new_rules'] as const
export type ChannelEvent = (typeof CHANNEL_EVENTS)[number]

export interface OrgChannel {
  id: string
  name: string
  kind: Tolerant<ChannelKind>
  url: string
  enabled: boolean
  createdAt: string
  hasSecret?: boolean
  email?: string
  project?: string
  hasToken?: boolean
  recipients?: string[]
  events?: Tolerant<ChannelEvent>[]
  unreadable?: boolean
}

export type KnownOrgChannel = OrgChannel & {kind: ChannelKind}

export interface OrgChannelInput {
  id?: string
  name?: string
  kind?: ChannelKind
  url?: string
  urlUnchanged?: boolean
  secret?: string
  clear?: 'secret'[]
  enabled?: boolean
  email?: string
  token?: string
  project?: string
  recipients?: string[]
  events?: ChannelEvent[]
}

export interface ChannelOptions {
  kinds: Tolerant<ChannelKind>[]
  events: Tolerant<ChannelEvent>[]
  defaultEvents: Tolerant<ChannelEvent>[]
  hostsRestricted: boolean
  allowedHosts?: string[]
}

const typedSecret = (kind: ChannelKind, secret: string): string | undefined =>
  channelSigns(kind) ? secret.trim() || undefined : undefined

export interface ChannelFields {
  email?: string
  token?: string
  project?: string
  recipients?: string[]
  events?: ChannelEvent[]
}

function kindFields(kind: ChannelKind, f: ChannelFields): OrgChannelInput {
  const body: OrgChannelInput = {}
  if (kind === 'jira') {
    const email = f.email?.trim()
    const token = f.token?.trim()
    const project = f.project?.trim()
    if (email) body.email = email
    if (token) body.token = token
    if (project) body.project = project
  }
  if (kind === 'email' && f.recipients) body.recipients = [...f.recipients]
  if (f.events) body.events = [...f.events]
  return body
}

export function channelCreateBody(
  input: {
    name: string
    kind: ChannelKind
    url: string
    secret: string
  } & ChannelFields,
): OrgChannelInput {
  const secret = typedSecret(input.kind, input.secret)
  return {
    name: input.name.trim(),
    kind: input.kind,
    ...(channelHasUrl(input.kind) ? {url: input.url.trim()} : {}),
    ...(secret ? {secret} : {}),
    ...kindFields(input.kind, input),
  }
}

export interface OrgChannelEdit extends ChannelFields {
  name: string
  url: string
  secret: string
  clearSecret: boolean
}

export function channelEditBody(ch: KnownOrgChannel, edit: OrgChannelEdit): OrgChannelInput {
  const url = edit.url.trim()
  const body: OrgChannelInput = {
    id: ch.id,
    name: edit.name.trim(),
    kind: ch.kind,
    ...(!channelHasUrl(ch.kind) ? {} : url ? {url} : {url: ch.url, urlUnchanged: true}),
    enabled: ch.enabled,
    ...kindFields(ch.kind, edit),
  }
  if (!channelSigns(ch.kind)) return body
  if (edit.clearSecret) return {...body, clear: ['secret']}
  const secret = typedSecret(ch.kind, edit.secret)
  return secret ? {...body, secret} : body
}

export const channelToggleBody = (ch: KnownOrgChannel, enabled: boolean): OrgChannelInput => ({
  id: ch.id,
  name: ch.name,
  kind: ch.kind,
  ...(channelHasUrl(ch.kind) ? {url: ch.url, urlUnchanged: true} : {}),
  enabled,
})

export const channelsApi = {
  list: async (orgId: string, signal?: AbortSignal): Promise<OrgChannel[]> =>
    requestValidated(`/api/orgs/${orgId}/channels`, await getOrgChannelsSchema(), {
      method: 'GET',
      noDedup: true,
      signal,
    }),

  options: async (orgId: string, signal?: AbortSignal): Promise<ChannelOptions | null> => {
    try {
      return await requestValidated(`/api/orgs/${orgId}/channels/options`, await getChannelOptionsSchema(), {
        method: 'GET',
        signal,
      })
    } catch (e) {
      if (e instanceof ApiError && (e.status === 404 || e.status === 405)) return null
      throw e
    }
  },

  save: (orgId: string, ch: OrgChannelInput): Promise<OrgChannel> => request(`/api/orgs/${orgId}/channels`, {body: ch}),

  remove: (orgId: string, channelId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/channels/${channelId}`, {method: 'DELETE', body: {}}),

  test: (orgId: string, channelId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/channels/${channelId}/test`, {body: {}}),
}

export interface OrgCICredential {
  id: string
  name: string
  secretHint: string
  createdBy: string
  createdByName?: string
  createdAt: string
  expiresAt?: string | null
  lastUsedAt?: string | null
}

export interface CreatedOrgCICredential extends OrgCICredential {
  credential: string
}

export interface OrgCICredentialInput {
  name: string
  expiresInDays?: number
}

export const ciCredentialsApi = {
  list: async (orgId: string, signal?: AbortSignal): Promise<OrgCICredential[]> =>
    requestValidated(`/api/orgs/${orgId}/ci-credentials`, await getOrgCICredentialsSchema(), {
      method: 'GET',
      noDedup: true,
      signal,
    }),

  create: async (orgId: string, input: OrgCICredentialInput): Promise<CreatedOrgCICredential> =>
    requestValidated(`/api/orgs/${orgId}/ci-credentials`, await getCreatedOrgCICredentialSchema(), {body: input}),

  revoke: (orgId: string, credId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/ci-credentials/${credId}`, {method: 'DELETE', body: {}}),
}

export interface OrgCustomRuleConfig {
  kind?: string
  schemaVersion?: number
  id: string
  name: string
  description?: string
  severity: 'error' | 'warning' | 'info'
  category?: string
  rawTypeMatch?: string
  nameMatch?: string
  typeMatch?: string
  propertyHas?: Record<string, string>
  propertyNot?: Record<string, string>
  suggestion?: string
  autoFix?: string
}

export interface OrgCustomRule {
  id: string
  ruleId: string
  config: OrgCustomRuleConfig
  enabled: boolean
  createdAt: string
  updatedAt: string
  problem?: string
  problemReason?: string
}

export const isNewerRule = (rule: OrgCustomRule): boolean =>
  rule.problemReason === 'UNKNOWN_KIND' ||
  (rule.problemReason === 'UNSUPPORTED_VERSION' && (rule.config.schemaVersion ?? 1) > 1)

export const orgRulesApi = {
  list: (orgId: string): Promise<OrgCustomRule[]> =>
    request(`/api/orgs/${orgId}/rules`, {method: 'GET', noDedup: true}),

  save: (orgId: string, config: OrgCustomRuleConfig, enabled = true, rowId?: string): Promise<OrgCustomRule> =>
    request(`/api/orgs/${orgId}/rules`, {body: {id: rowId, config, enabled}}),

  setEnabled: async (
    orgId: string,
    rowId: string,
    enabled: boolean,
    config?: OrgCustomRuleConfig,
  ): Promise<OrgCustomRule> => {
    try {
      return await request<OrgCustomRule>(`/api/orgs/${orgId}/rules`, {body: {id: rowId, enabled}})
    } catch (e) {
      if (config && e instanceof ApiError && e.status === 400 && e.message.includes('config.id is required')) {
        return request<OrgCustomRule>(`/api/orgs/${orgId}/rules`, {body: {id: rowId, config, enabled}})
      }
      throw e
    }
  },

  remove: (orgId: string, rowId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/rules/${rowId}`, {method: 'DELETE', body: {}}),
}
