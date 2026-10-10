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
// Org admins configure their own webhook/Teams/Slack/Jira/email destinations;
// governance events for the org's flows are delivered there in addition to the
// deployment-global channels.

export const CHANNEL_KINDS = ['webhook', 'teams', 'teams-workflow', 'slack', 'jira', 'email'] as const
export type ChannelKind = (typeof CHANNEL_KINDS)[number]

/** The kinds a server before the Jira and email organisation channels offers (it has no options route). */
export const LEGACY_CHANNEL_KINDS: readonly ChannelKind[] = ['webhook', 'teams', 'teams-workflow', 'slack']

/**
 * The kinds that sign their deliveries with an HMAC secret. A Teams channel, of either kind, has none: the server
 * refuses one.
 */
export const SIGNED_CHANNEL_KINDS: readonly ChannelKind[] = ['webhook', 'slack']

export const channelSigns = (kind: ChannelKind): boolean => SIGNED_CHANNEL_KINDS.includes(kind)

/** An email channel goes to chosen members, not to a URL. */
export const channelHasUrl = (kind: ChannelKind): boolean => kind !== 'email'

/** The event types an organisation channel can subscribe to (the test event reaches every channel). */
export const CHANNEL_EVENTS = ['drift', 'health_regression', 'new_rules'] as const
export type ChannelEvent = (typeof CHANNEL_EVENTS)[number]

export interface OrgChannel {
  id: string
  name: string
  /** A kind this bundle does not know is listed as sent; it can be tested and removed here, not edited. */
  kind: Tolerant<ChannelKind>
  /**
   * Where events go, redacted to `host/…`: for Slack and Teams the URL is the credential. It has no scheme, so it is
   * a display value, never a URL to validate or open. A server before the channel rework lists the full URL to
   * admins. Either way, an edit re-sends it as "unchanged".
   */
  url: string
  enabled: boolean
  createdAt: string
  /** A signing secret is set (the secret itself is never returned). Absent from an older server: unknown. */
  hasSecret?: boolean
  /** Jira: the account's email and the project key. */
  email?: string
  project?: string
  /** Jira: the API token is set (it is never returned). */
  hasToken?: boolean
  /** Email: the members (user ids) it goes to, each at their account's verified address. */
  recipients?: string[]
  /** The event types it receives. Absent from a server before subscriptions: every organisation event. */
  events?: Tolerant<ChannelEvent>[]
  /** Its stored URL cannot be decrypted (the server's encryption key changed): enter the URL again or remove it. */
  unreadable?: boolean
}

export type KnownOrgChannel = OrgChannel & {kind: ChannelKind}

// The save body. An edit (an `id` of a saved channel) keeps every field it omits: `secret` (or `token`) omitted keeps
// the stored one, `clear: ['secret']` removes the secret, and `urlUnchanged` (or the listed `url` itself) keeps the
// stored URL. A server before the Jira and email channels ignores the fields it does not know.
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

/** What the channel form needs from the server (GET …/channels/options, admins). */
export interface ChannelOptions {
  /** The kinds this deployment delivers (email only with mail settings). */
  kinds: Tolerant<ChannelKind>[]
  events: Tolerant<ChannelEvent>[]
  defaultEvents: Tolerant<ChannelEvent>[]
  /** A channel URL must be on the operator's host list. */
  hostsRestricted: boolean
  /** That list, when the operator made it visible; else absent. */
  allowedHosts?: string[]
}

// The secret is an HMAC key: whitespace around a pasted one is a slip, and a
// blank one is no secret (sending "" would read as "keep" anyway).
const typedSecret = (kind: ChannelKind, secret: string): string | undefined =>
  channelSigns(kind) ? secret.trim() || undefined : undefined

/** The kind fields beyond url and secret a form edits; each is sent only for its kind. */
export interface ChannelFields {
  /** Jira: the account's email, a new API token (blank keeps the stored one) and the project key. */
  email?: string
  token?: string
  project?: string
  /** Email: the members it goes to. */
  recipients?: string[]
  /** The events it receives; undefined leaves the choice as it is. */
  events?: ChannelEvent[]
}

// kindFields is the part of a save body for kind's own fields: Jira's typed values (a blank one keeps the stored
// value), an email channel's recipients, and the events when chosen.
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
  /** A new URL; blank keeps the stored one. */
  url: string
  /** A new signing secret; blank keeps the stored one. */
  secret: string
  /** Remove the stored signing secret (wins over a typed one). */
  clearSecret: boolean
}

// channelEditBody and channelToggleBody also carry what the previous server
// needs to keep a channel as it is: it requires the kind, stores the name as
// sent, and enables a channel sent without `enabled`. It lists the full URL to
// admins, so re-sending the listed URL keeps it there; this server reads that
// (or `urlUnchanged`) as "unchanged". (That server still drops the secret on
// every save; nothing a client sends prevents it.)
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
  // noDedup: the list is re-read right after a save, inside the 5 s GET micro-cache window.
  list: async (orgId: string, signal?: AbortSignal): Promise<OrgChannel[]> =>
    requestValidated(`/api/orgs/${orgId}/channels`, await getOrgChannelsSchema(), {
      method: 'GET',
      noDedup: true,
      signal,
    }),

  // The form's choices (admins). A server before them has no such route: null, and the form offers the legacy kinds.
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

  // Unvalidated: the section re-reads the list after a save, and a drifted answer must not report a stored save as failed.
  save: (orgId: string, ch: OrgChannelInput): Promise<OrgChannel> => request(`/api/orgs/${orgId}/channels`, {body: ch}),

  remove: (orgId: string, channelId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/channels/${channelId}`, {method: 'DELETE', body: {}}),

  // Synchronous probe: 200 = delivered. 502 CHANNEL_DELIVERY_FAILED carries details.reason (and details.status for
  // an HTTP answer); the message of a 5xx is masked. 503 CHANNEL_DELIVERIES_BUSY; 429 its own rate limit.
  test: (orgId: string, channelId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/channels/${channelId}/test`, {body: {}}),
}

// ── Org CI credentials (R8) ─────────────────────────────────────────────────
// Org admins mint the credentials `bakicli report` signs its CI webhook calls
// with (PAD_CI_WEBHOOK_SECRET). The server keeps them sealed and returns one
// only in its create answer; reads carry its last four characters.

export interface OrgCICredential {
  id: string
  name: string
  /** The credential's last four characters. */
  secretHint: string
  /** The creating admin's user id; '' once that account was erased. */
  createdBy: string
  /** The creating admin's display name or email as the server knows it; '' once that account was erased. */
  createdByName?: string
  createdAt: string
  /** Null or absent: never expires. */
  expiresAt?: string | null
  /** Null or absent: never used. */
  lastUsedAt?: string | null
}

export interface CreatedOrgCICredential extends OrgCICredential {
  /** The full credential (`baki_ci_<keyId>_<secret>`). Shown once: no read returns it again. */
  credential: string
}

export interface OrgCICredentialInput {
  name: string
  /** 1..CI_CREDENTIAL_MAX_EXPIRY_DAYS; omitted = never expires. */
  expiresInDays?: number
}

export const CI_CREDENTIAL_MAX_EXPIRY_DAYS = 365
/** The server refuses a longer name (400). */
export const CI_CREDENTIAL_MAX_NAME = 100

export const ciCredentialsApi = {
  // noDedup: the list is re-read right after a create or revoke, inside the
  // 5 s GET micro-cache window.
  list: async (orgId: string, signal?: AbortSignal): Promise<OrgCICredential[]> =>
    requestValidated(`/api/orgs/${orgId}/ci-credentials`, await getOrgCICredentialsSchema(), {
      method: 'GET',
      noDedup: true,
      signal,
    }),

  // 409: the org's cap of active credentials; 503: the server cannot seal secrets (field encryption off).
  create: async (orgId: string, input: OrgCICredentialInput): Promise<CreatedOrgCICredential> =>
    requestValidated(`/api/orgs/${orgId}/ci-credentials`, await getCreatedOrgCICredentialSchema(), {body: input}),

  // 204 No Content.
  revoke: (orgId: string, credId: string): Promise<void> =>
    request(`/api/orgs/${orgId}/ci-credentials/${credId}`, {method: 'DELETE', body: {}}),
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
  // noDedup: the list is re-read right after a save, pause or delete, inside the 5 s GET micro-cache window.
  list: (orgId: string): Promise<OrgCustomRule[]> =>
    request(`/api/orgs/${orgId}/rules`, {method: 'GET', noDedup: true}),

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
