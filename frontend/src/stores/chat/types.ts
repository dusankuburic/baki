import type {ProviderID, ToolCallRecord} from '@/types'
import type {KnowledgeStatus} from '@/lib/chatEvent'
import type {BlockAnchor} from '@/lib/blockAnchor'

export const MAX_CONCURRENT_STREAMS = 3

export const MAX_THREADS = 50

/** Follow-ups a thread can hold while its reply streams (R-frontend-13); queueing one more is refused. */
export const MAX_QUEUED_MESSAGES = 5

/**
 * Why a thread's queue holds instead of sending its next follow-up: the turn before it failed ('error'), the user
 * stopped it ('stopped'), or the engine refused the queued message ('refused'). The items stay; the user resumes.
 */
export type QueuePause = 'error' | 'stopped' | 'refused'

export function conversationKey(thread: Pick<ChatThread, 'id'>): string {
  return thread.id
}

export interface ChatThread {
  id: string
  flowId: string
  title: string
  createdAt: string
  contextBlockId: string | null
  contextBlockAnchor?: BlockAnchor | null
  /** Set once a re-parse lost the context block: its name, or '' when no parse could name it. */
  contextBlockLost?: string
  selectedSourceFiles: string[]
  tokensIn: number
  tokensOut: number
  useTools?: boolean
  usedTools?: boolean
  appliedFixes?: boolean
  provider?: ProviderID
  model?: string
  modelLabel?: string
  inFlight?: InFlightTurn
}

export interface InFlightTurn {
  streamId: string
  messageId?: string
  provider: ProviderID
  model: string
  modelLabel?: string
}

export interface ThreadModel {
  provider: ProviderID
  model: string
  modelLabel?: string
}

export type ThreadPinFields = Pick<ChatThread, 'provider' | 'model' | 'modelLabel'>

export function threadPinKey(thread: Pick<ChatThread, 'provider' | 'model'> | undefined): string {
  return thread?.provider && thread.model ? `${thread.provider}\u0000${thread.model}` : ''
}

export function threadModelOf(thread: Pick<ChatThread, 'provider' | 'model' | 'modelLabel'>): ThreadModel | null {
  if (!thread.provider || !thread.model) return null
  return {
    provider: thread.provider,
    model: thread.model,
    ...(thread.modelLabel ? {modelLabel: thread.modelLabel} : {}),
  }
}

export interface ModelRefusal extends ThreadModel {
  epoch: number
}

export interface ApprovalItem {
  id: string
  label: string
  summary: string
  status: 'pending' | 'applied' | 'applied-unresolved' | 'error' | 'already-resolved' | 'skipped'
  message?: string
  ruleId?: string
  fixType?: string
  blockLabel?: string
  line?: number
}

export interface ApprovalCard {
  proposalId: string
  tool: string
  previewKind: string
  source: 'fix' | 'approval'
  requiredScope?: string
  status:
    | 'pending'
    | 'applying'
    | 'applied'
    | 'applied-unresolved'
    | 'already-resolved'
    | 'declined'
    | 'timeout'
    | 'error'
    | 'cancelled'
  message?: string
  items: ApprovalItem[]
  ruleId?: string
  fixType?: string
  blockLabel?: string
  line?: number
  summary?: string
  receivedAt?: number
}

export type FixProposalItem = ApprovalItem
export type FixProposalCard = ApprovalCard

export interface StreamSlot {
  streamId: string
  messageId: string
  text: string
  isThinking: boolean
  tokens: number
  toolStatus: string | null
  toolCalls: ToolCallRecord[]
  approvals: ApprovalCard[]
  /** Earlier history messages the server left out of this turn so far (the latest history_truncated notice). */
  truncated?: number
  /** Why the turn went without the org's knowledge base (its knowledge notice). */
  knowledge?: KnowledgeStatus
}

/**
 * A follow-up typed while the thread's reply streams. It lives in memory only (not persisted: a reload drops the
 * queue), and is sent at drain time with the thread's context as it is then (context block, tools, source files
 * unless it tagged its own).
 */
export interface QueuedMessage {
  /** Names the item for a cancel; queueMessage assigns it. */
  id: string
  text: string
  files: string[]
  excludeContext?: boolean
  provider?: ProviderID
  model?: string
  modelLabel?: string
  pinThread?: boolean
  pinnedAs?: string
}

export type HistoryState = 'loading' | 'loaded' | 'error'

export interface HistoryStatus {
  state: HistoryState
  token: number
  /** The server's version of the thread's stored conversation; unknown (absent) sends saves without a base version. */
  version?: number
}
