
export type ChatFailureKind = 'capacity' | 'modelNotAvailable' | 'serverRestarting' | 'generic'

export const CHAT_ERROR_CODES = {
  capacity: 'CHAT_CAPACITY_REACHED',
  modelNotAvailable: 'MODEL_NOT_AVAILABLE',
  serverRestarting: 'SERVER_RESTARTING',
  interrupted: 'STREAM_INTERRUPTED',
  /** The server's watchdog stopped a stream that made no progress; the message names the phase that stalled (setup,
   * the provider, a tool, an approval, an apply). A generic failure: the bubble shows that copy, with Retry. */
  idleTimeout: 'STREAM_IDLE_TIMEOUT',
  /** The server's watchdog stopped a stream whose client had no live event connection. A generic failure. */
  clientGone: 'STREAM_CLIENT_GONE',
  replicaUnreachable: 'REPLICA_UNREACHABLE',
  streamIdInUse: 'STREAM_ID_IN_USE',
  contextLimit: 'CONTEXT_LIMIT',
  toolLoopLimit: 'TOOL_LOOP_LIMIT',
  /** Client-side: the desktop backend process was replaced while the stream ran (R-frontend-c7). */
  backendRestarted: 'BACKEND_RESTARTED',
} as const

/** What an error bubble offers for a server code that is not a provider error, beside Retry: Compact and a hint
 * for a context overflow (compacting helps when the history is what overflowed; when the current question or its
 * tool results are, it must be narrowed), a hint for the tool loop's step limit. */
export interface ChatErrorGuidance {
  action?: 'compact'
  hintKey?: 'errors.toolLoopLimitHint' | 'errors.contextLimitHint'
  hideRetry: boolean
}

export function chatErrorGuidance(code?: string | null): ChatErrorGuidance | null {
  switch (code) {
    case CHAT_ERROR_CODES.contextLimit:
      return {action: 'compact', hintKey: 'errors.contextLimitHint', hideRetry: false}
    case CHAT_ERROR_CODES.toolLoopLimit:
      return {hintKey: 'errors.toolLoopLimitHint', hideRetry: false}
    default:
      return null
  }
}

export function classifyChatFailure(message: string, code?: string | null): ChatFailureKind {
  if (code === CHAT_ERROR_CODES.capacity) return 'capacity'
  if (code === CHAT_ERROR_CODES.modelNotAvailable) return 'modelNotAvailable'
  if (code === CHAT_ERROR_CODES.serverRestarting) return 'serverRestarting'
  if (code) return 'generic'
  if (message.includes('too many chat responses')) return 'capacity'
  return 'generic'
}

/** The server running the stream could not be reached by the one that got the request (503): nothing was stopped or decided, so the request may be sent again. */
export function isReplicaUnreachable(err: unknown): boolean {
  return (
    typeof err === 'object' && err !== null && (err as {code?: unknown}).code === CHAT_ERROR_CODES.replicaUnreachable
  )
}

/** A request a draining server refused (503 SERVER_RESTARTING): nothing was done, and it may be sent again. */
export function isServerRestarting(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as {code?: unknown}).code === CHAT_ERROR_CODES.serverRestarting
}

export function isStreamGone(err: unknown): boolean {
  const status = typeof err === 'object' && err !== null ? (err as {status?: unknown}).status : undefined
  return status === 403 || status === 404 || status === 410
}

export function retryAfterHint(err: unknown): number | undefined {
  const details = typeof err === 'object' && err !== null ? (err as {details?: unknown}).details : undefined
  const v =
    typeof details === 'object' && details !== null
      ? (details as {retryAfterSeconds?: unknown}).retryAfterSeconds
      : undefined
  return typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : undefined
}

const SERVER_RESTART_RETRY_S = 2
const SERVER_RESTART_RETRY_MAX_S = 10
const SERVER_RESTART_JITTER_MS = 1_000

export function serverRestartDelayMs(retryAfterSeconds?: number, random: () => number = Math.random): number {
  const s =
    retryAfterSeconds !== undefined && retryAfterSeconds > 0
      ? Math.min(retryAfterSeconds, SERVER_RESTART_RETRY_MAX_S)
      : SERVER_RESTART_RETRY_S
  return s * 1000 + Math.floor(random() * SERVER_RESTART_JITTER_MS)
}

export type ProviderErrorCode =
  | 'PROVIDER_AUTH'
  | 'PROVIDER_PERMISSION'
  | 'PROVIDER_NOT_CONFIGURED'
  | 'PROVIDER_UNAVAILABLE'
  | 'PROVIDER_CONTENT_FILTERED'
  | 'PROVIDER_OUTPUT_LIMIT'
  | 'PROVIDER_BALANCE'
  | 'PROVIDER_UNKNOWN'
  | 'PROVIDER_NOT_ALLOWED'
  | 'MODEL_NOT_AVAILABLE'

export interface ProviderErrorInfo {
  titleKey:
    | 'errors.providerAuthTitle'
    | 'errors.providerPermissionTitle'
    | 'errors.providerNotConfiguredTitle'
    | 'errors.providerUnavailableTitle'
    | 'errors.providerContentFilteredTitle'
    | 'errors.providerOutputLimitTitle'
    | 'errors.providerBalanceTitle'
    | 'errors.providerUnknownTitle'
    | 'errors.providerNotAllowedTitle'
    | 'errors.modelNotAvailable'
  settingsSection: 'accounts' | 'behavior'
  refreshProviders: boolean
  action?: 'chooseModel'
}

const PROVIDER_ERRORS: Record<ProviderErrorCode, ProviderErrorInfo> = {
  PROVIDER_AUTH: {titleKey: 'errors.providerAuthTitle', settingsSection: 'accounts', refreshProviders: true},
  PROVIDER_PERMISSION: {
    titleKey: 'errors.providerPermissionTitle',
    settingsSection: 'accounts',
    refreshProviders: false,
  },
  PROVIDER_NOT_CONFIGURED: {
    titleKey: 'errors.providerNotConfiguredTitle',
    settingsSection: 'accounts',
    refreshProviders: true,
  },
  PROVIDER_UNAVAILABLE: {
    titleKey: 'errors.providerUnavailableTitle',
    settingsSection: 'accounts',
    refreshProviders: false,
  },
  PROVIDER_CONTENT_FILTERED: {
    titleKey: 'errors.providerContentFilteredTitle',
    settingsSection: 'accounts',
    refreshProviders: false,
  },
  PROVIDER_OUTPUT_LIMIT: {
    titleKey: 'errors.providerOutputLimitTitle',
    settingsSection: 'behavior',
    refreshProviders: false,
  },
  PROVIDER_BALANCE: {titleKey: 'errors.providerBalanceTitle', settingsSection: 'accounts', refreshProviders: false},
  PROVIDER_UNKNOWN: {
    titleKey: 'errors.providerUnknownTitle',
    settingsSection: 'accounts',
    refreshProviders: true,
    action: 'chooseModel',
  },
  PROVIDER_NOT_ALLOWED: {
    titleKey: 'errors.providerNotAllowedTitle',
    settingsSection: 'accounts',
    refreshProviders: true,
    action: 'chooseModel',
  },
  MODEL_NOT_AVAILABLE: {
    titleKey: 'errors.modelNotAvailable',
    settingsSection: 'accounts',
    refreshProviders: true,
    action: 'chooseModel',
  },
}

export function providerErrorInfo(code?: string | null): ProviderErrorInfo | null {
  if (!code || !Object.prototype.hasOwnProperty.call(PROVIDER_ERRORS, code)) return null
  return PROVIDER_ERRORS[code as ProviderErrorCode]
}
