// Classifies chat failures for their user-facing treatment. Two paths report
// failures — the create POST (an ApiError with an envelope `code`) and the
// stream's SSE `error` event (a `code` since the backend started sending one)
// — and both must land on the same UX: a busy toast for the concurrency cap,
// the dedicated budget notice for the daily AI budget, the thread's model
// notice for a model that is gone, one automatic resend for a server restart,
// a generic error bubble for everything else. The message regexes are the
// fallback for older backends that send no code.

export type ChatFailureKind = 'capacity' | 'budget' | 'modelNotAvailable' | 'serverRestarting' | 'generic'

// Mirrors the backend codes in internal/service/chat_errors.go and the
// create handler's CHAT_CAPACITY_REACHED / MODEL_NOT_AVAILABLE.
export const CHAT_ERROR_CODES = {
  capacity: 'CHAT_CAPACITY_REACHED',
  budgetExceeded: 'AI_BUDGET_EXCEEDED',
  // The thread's model is not offered to the caller any more (removed, or
  // its organisation no longer shares it): the create request answers 400
  // with it before anything is streamed.
  modelNotAvailable: 'MODEL_NOT_AVAILABLE',
  // The server is draining for a restart (a deploy): it ends each running
  // stream with this code, and refuses a new one with a 503 carrying it.
  // The engine sends such a turn again once (stream-engine/lifecycle).
  serverRestarting: 'SERVER_RESTARTING',
  // The stream is lost: the reconnected server does not know it (another
  // instance ran it), so nothing of it can be resumed.
  interrupted: 'STREAM_INTERRUPTED',
} as const

export function classifyChatFailure(message: string, code?: string | null): ChatFailureKind {
  if (code === CHAT_ERROR_CODES.capacity) return 'capacity'
  if (code === CHAT_ERROR_CODES.budgetExceeded) return 'budget'
  if (code === CHAT_ERROR_CODES.modelNotAvailable) return 'modelNotAvailable'
  if (code === CHAT_ERROR_CODES.serverRestarting) return 'serverRestarting'
  if (code) return 'generic'
  if (message.includes('too many chat responses')) return 'capacity'
  if (/budget exceeded/i.test(message)) return 'budget'
  return 'generic'
}

// budgetDetail extracts the "$spent / $limit" figures from a budget message
// as a parenthesised suffix for the notice copy, or '' when absent.
export function budgetDetail(message: string): string {
  const amounts = message.match(/\$[0-9.]+\s*\/\s*\$[0-9.]+/)
  return amounts ? ` (${amounts[0]})` : ''
}

// isStreamGone reports a resume failure that means the server does not have
// the stream: 403 (under JWT an unknown stream is "not yours"), 404, 410.
// Anything else (the network, a 5xx while a new instance starts) may pass.
// Reads `status` structurally, as ApiError carries it.
export function isStreamGone(err: unknown): boolean {
  const status = typeof err === 'object' && err !== null ? (err as {status?: unknown}).status : undefined
  return status === 403 || status === 404 || status === 410
}

// retryAfterHint is the Retry-After (seconds) of a SERVER_RESTARTING refusal
// as the error envelope's details.retryAfterSeconds: ApiError does not carry
// the response headers. Undefined without a positive number.
export function retryAfterHint(err: unknown): number | undefined {
  const details = typeof err === 'object' && err !== null ? (err as {details?: unknown}).details : undefined
  const v =
    typeof details === 'object' && details !== null
      ? (details as {retryAfterSeconds?: unknown}).retryAfterSeconds
      : undefined
  return typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : undefined
}

// The server's own Retry-After while draining is 2 s; a larger hint is capped
// so a turn never waits long on a status line, and the jitter spreads the
// clients of one deploy over a second.
const SERVER_RESTART_RETRY_S = 2
const SERVER_RESTART_RETRY_MAX_S = 10
const SERVER_RESTART_JITTER_MS = 1_000

// serverRestartDelayMs is how long the engine waits before sending a turn
// the restart cut short again.
export function serverRestartDelayMs(retryAfterSeconds?: number, random: () => number = Math.random): number {
  const s =
    retryAfterSeconds !== undefined && retryAfterSeconds > 0
      ? Math.min(retryAfterSeconds, SERVER_RESTART_RETRY_MAX_S)
      : SERVER_RESTART_RETRY_S
  return s * 1000 + Math.floor(random() * SERVER_RESTART_JITTER_MS)
}

// Provider failures the user fixes in Settings (credentials, role, balance,
// Max tokens) or by choosing another model, or should at least know the
// class of. Each gets a localized title, the server's message as detail, and
// an action on the error bubble: "Open settings" at the section that fixes
// it, or "Choose another model" (opens the chat's model picker).
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
  // chat namespace key of the bubble's title.
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
  // Settings section the "Open settings" action opens.
  settingsSection: 'accounts' | 'behavior'
  // The provider listing is stale (a key was revoked or removed elsewhere,
  // a model was removed): refetch it so the picker stops offering what
  // cannot answer.
  refreshProviders: boolean
  // The bubble's action when it is not "Open settings": 'chooseModel' opens
  // the chat's model picker ("Choose another model").
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
  // The server's copy says to raise Max tokens in AI Behavior.
  PROVIDER_OUTPUT_LIMIT: {
    titleKey: 'errors.providerOutputLimitTitle',
    settingsSection: 'behavior',
    refreshProviders: false,
  },
  PROVIDER_BALANCE: {titleKey: 'errors.providerBalanceTitle', settingsSection: 'accounts', refreshProviders: false},
  // The thread's provider is not in this server's registry (a pin a newer
  // bundle wrote, a provider the deployment dropped): relist, and offer
  // another model — AI Accounts cannot add what the server does not know.
  PROVIDER_UNKNOWN: {
    titleKey: 'errors.providerUnknownTitle',
    settingsSection: 'accounts',
    refreshProviders: true,
    action: 'chooseModel',
  },
  // The provider policy refused the thread's provider (disabled in this
  // deployment, or off the organisation's allowed-provider list): the server's
  // message says which; relist, and offer another model — nothing in AI
  // Accounts enables it.
  PROVIDER_NOT_ALLOWED: {
    titleKey: 'errors.providerNotAllowedTitle',
    settingsSection: 'accounts',
    refreshProviders: true,
    action: 'chooseModel',
  },
  // The model was removed (or stopped being shared) after the picker listed
  // it. On the create request the engine returns the message to the
  // composer instead (see useChatStreamEngine); a bubble only shows when the
  // stream itself reports it.
  MODEL_NOT_AVAILABLE: {
    titleKey: 'errors.modelNotAvailable',
    settingsSection: 'accounts',
    refreshProviders: true,
    action: 'chooseModel',
  },
}

// providerErrorInfo returns the treatment for a provider failure code, or
// null for any other (or no) code.
export function providerErrorInfo(code?: string | null): ProviderErrorInfo | null {
  if (!code || !Object.prototype.hasOwnProperty.call(PROVIDER_ERRORS, code)) return null
  return PROVIDER_ERRORS[code as ProviderErrorCode]
}
