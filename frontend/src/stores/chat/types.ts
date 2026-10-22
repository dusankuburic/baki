import type {ProviderID, ToolCallRecord} from '@/types'
import type {BlockAnchor} from '@/lib/blockAnchor'

export const MAX_CONCURRENT_STREAMS = 3

export const MAX_THREADS = 50

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
  status: 'pending' | 'applied' | 'applied-unresolved' | 'error' | 'already-resolved'
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
  status: 'pending' | 'applying' | 'applied' | 'applied-unresolved' | 'declined' | 'timeout' | 'error' | 'cancelled'
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
}

export interface QueuedMessage {
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
