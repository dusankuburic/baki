import {ApiError, request, requestValidated} from './client'
import {getGovernanceAlertsSchema, getUnreadCountSchema} from './schemas'
import type {Tolerant} from '@/types'

export type GovernanceAlertType = 'drift' | 'health_regression' | 'finding_assigned' | 'comment_added' | 'new_rules'
/** Whose "new rules" notice: an organisation's, a user's own flows, or the whole deployment without accounts. */
export type GovernanceNoticeScope = 'org' | 'user' | 'app'
export type GovernanceAlertSeverity = 'error' | 'warning'

// A governance alert row, mirroring storageif.GovernanceAlert on the backend.
// The server writes the title and message, so an alert of a type or severity
// this bundle does not know still reads; only its chip falls back.
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
  /** Personal delivery target (assignment/comment alerts); empty = team-wide. */
  targetUser?: string
  /** A new_rules notice (flowId ""): the rules newer than the flows' baselines that report findings, sorted. */
  ruleIds?: string[]
  scope?: Tolerant<GovernanceNoticeScope>
  /** Flows of the scope where those rules report findings. */
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

// ── Org notification channels (R2-3) ────────────────────────────────────────
// Org admins configure their own webhook/Teams/Slack destinations; governance
// events for the org's flows are delivered there in addition to the
// deployment-global channels.

export const CHANNEL_KINDS = ['webhook', 'teams', 'slack'] as const
export type ChannelKind = (typeof CHANNEL_KINDS)[number]

export interface OrgChannel {
  id: string
  name: string
  /** A kind this bundle does not know is listed as sent; it can be tested and removed here, not edited. */
  kind: Tolerant<ChannelKind>
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
  list: (orgId: string): Promise<OrgChannel[]> => request(`/api/orgs/${orgId}/channels`, {method: 'GET'}),

  save: (orgId: string, ch: OrgChannelInput): Promise<OrgChannel> => request(`/api/orgs/${orgId}/channels`, {body: ch}),

  remove: (orgId: string, channelId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/channels/${channelId}`, {method: 'DELETE', body: {}}),

  // Synchronous probe: 200 = delivered, non-2xx carries the failure.
  test: (orgId: string, channelId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/channels/${channelId}/test`, {body: {}}),
}

/**
 * A user-authored analyzer rule owned by an org (R4); the format is
 * docs/custom-rules.md.
 *
 * `config.id` is the rule id that appears on findings and is unique PER ORG —
 * two orgs may both define `house-style` and mean different things. `id` is the
 * storage row's surrogate key, used for delete. A saved rule's `config.id`
 * cannot change (the server answers 409).
 */
export interface OrgCustomRuleConfig {
  /** `block` (the only kind this version runs); the server echoes it spelled out. A newer release may write others. */
  kind?: string
  /** 1; the server echoes it spelled out. */
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
  /** Why the analysis skips this rule; absent when it runs. */
  problem?: string
  /** The analyzer's reason code for `problem`, e.g. INVALID_SEVERITY, UNKNOWN_KIND, UNSUPPORTED_VERSION, INVALID_JSON. */
  problemReason?: string
}

/** A rule a newer release of Baki wrote: this version cannot run or edit it (pause and delete still work). */
export const isNewerRule = (rule: OrgCustomRule): boolean =>
  rule.problemReason === 'UNKNOWN_KIND' ||
  (rule.problemReason === 'UNSUPPORTED_VERSION' && (rule.config.schemaVersion ?? 1) > 1)

export const orgRulesApi = {
  list: (orgId: string): Promise<OrgCustomRule[]> => request(`/api/orgs/${orgId}/rules`, {method: 'GET'}),

  save: (orgId: string, config: OrgCustomRuleConfig, enabled = true, rowId?: string): Promise<OrgCustomRule> =>
    request(`/api/orgs/${orgId}/rules`, {body: {id: rowId, config, enabled}}),

  /**
   * Pauses or resumes a saved rule without sending its config, so a rule that no longer validates can be paused.
   * A server from before rule schemas (a rollback while this bundle is open) toggles only with the config and
   * answers 400 "config.id is required"; then the toggle is resent with `config`, the old way.
   */
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
