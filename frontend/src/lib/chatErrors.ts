// Classifies chat failures for their user-facing treatment. Two paths report
// failures — the create POST (an ApiError with an envelope `code`) and the
// stream's SSE `error` event (a `code` since the backend started sending one)
// — and both must land on the same UX: a busy toast for the concurrency cap,
// the dedicated budget notice for the daily AI budget, a generic error bubble
// for everything else. The message regexes are the fallback for older
// backends that send no code.

export type ChatFailureKind = 'capacity' | 'budget' | 'generic'

// Mirrors the backend codes in internal/service/chat_errors.go and the
// create handler's CHAT_CAPACITY_REACHED.
export const CHAT_ERROR_CODES = {
  capacity: 'CHAT_CAPACITY_REACHED',
  budgetExceeded: 'AI_BUDGET_EXCEEDED',
} as const

export function classifyChatFailure(message: string, code?: string | null): ChatFailureKind {
  if (code === CHAT_ERROR_CODES.capacity) return 'capacity'
  if (code === CHAT_ERROR_CODES.budgetExceeded) return 'budget'
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

// Provider failures the user fixes in Settings (credentials, role, balance,
// Max tokens) or should at least know the class of. Each gets a localized
// title, the server's message as detail, and an "Open settings" action on the
// error bubble pointing at the section that fixes it.
export type ProviderErrorCode =
  | 'PROVIDER_AUTH'
  | 'PROVIDER_PERMISSION'
  | 'PROVIDER_NOT_CONFIGURED'
  | 'PROVIDER_UNAVAILABLE'
  | 'PROVIDER_CONTENT_FILTERED'
  | 'PROVIDER_OUTPUT_LIMIT'
  | 'PROVIDER_BALANCE'

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
  // Settings section the "Open settings" action opens.
  settingsSection: 'accounts' | 'behavior'
  // The provider listing is stale (a key was revoked or removed elsewhere):
  // refetch it so the picker stops offering a provider that cannot answer.
  refreshProviders: boolean
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
}

// providerErrorInfo returns the treatment for a provider failure code, or
// null for any other (or no) code.
export function providerErrorInfo(code?: string | null): ProviderErrorInfo | null {
  if (!code || !Object.prototype.hasOwnProperty.call(PROVIDER_ERRORS, code)) return null
  return PROVIDER_ERRORS[code as ProviderErrorCode]
}
