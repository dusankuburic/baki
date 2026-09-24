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
