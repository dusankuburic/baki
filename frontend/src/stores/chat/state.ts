import type {ChatMessage, FlowDocument, ToolCallRecord} from '@/types'
import type {
  ChatThread,
  FixProposalCard,
  FixProposalItem,
  HistoryStatus,
  ModelRefusal,
  QueuedMessage,
  QueuePause,
  StreamSlot,
} from './types'

export interface ChatState {
  threads: ChatThread[]
  activeThreadId: string | null
  conversations: Map<string, ChatMessage[]>
  streams: Record<string, StreamSlot>
  providerEpoch: number
  modelPickerRequest: number
  openModelPicker: () => void
  modelRefusals: Record<string, ModelRefusal>
  setModelRefusal: (threadId: string, refusal: ModelRefusal | null) => void
  stagedPrompt: {threadId: string; text: string} | null
  drafts: Record<string, string>
  historyStatus: Record<string, HistoryStatus>
  beginHistoryLoad: (threadId: string) => number
  finishHistoryLoad: (threadId: string, token: number, loaded: ChatMessage[] | null, version?: number) => boolean
  markHistoryLoaded: (threadId: string) => void
  getConversationVersion: (threadId: string) => number | undefined
  setConversationVersion: (threadId: string, version: number) => void
  isHistorySynced: (threadId: string) => boolean

  getMessages: (threadId: string) => readonly ChatMessage[]
  appendMessage: (threadId: string, message: ChatMessage) => void
  removeMessage: (threadId: string, messageId: string) => void
  /** Replaces one message by what patch makes of it (no-op for an unknown message). */
  updateMessage: (threadId: string, messageId: string, patch: (m: ChatMessage) => ChatMessage) => void
  clearThreadMessages: (threadId: string) => void
  setThreadMessages: (threadId: string, messages: readonly ChatMessage[]) => void
  compactThread: (threadId: string, keepPairs: number) => void
  updateStreamingMessage: (threadId: string, text: string) => void
  startStream: (threadId: string, streamId: string, messageId: string) => void
  /** Each thread's follow-ups in send order (FIFO, at most MAX_QUEUED_MESSAGES); a thread with none has no key. */
  queuedByThread: Record<string, QueuedMessage[]>
  /** Threads whose queue holds (only while it has items); see QueuePause. */
  queuePausedByThread: Record<string, QueuePause>
  /** Holds a thread's queue; a thread with nothing queued is left alone. */
  pauseQueue: (threadId: string, reason: QueuePause) => void
  resumeQueue: (threadId: string) => void
  /** Puts a taken follow-up back at the head of the queue (past the cap: it was already queued). */
  requeueMessage: (threadId: string, msg: QueuedMessage) => void
  /** Appends a follow-up; false (and nothing queued) when the thread already holds MAX_QUEUED_MESSAGES. */
  queueMessage: (threadId: string, msg: Omit<QueuedMessage, 'id'>) => boolean
  /** Drops one queued follow-up by its id. */
  removeQueuedMessage: (threadId: string, id: string) => void
  /** Drops every queued follow-up of the thread. */
  clearQueuedMessage: (threadId: string) => void
  /** Removes and returns the oldest queued follow-up. */
  takeQueuedMessage: (threadId: string) => QueuedMessage | undefined
  addToolCall: (threadId: string, record: ToolCallRecord) => void
  endStream: (threadId: string) => void
  setStreamMeta: (
    threadId: string,
    patch: Partial<Pick<StreamSlot, 'isThinking' | 'tokens' | 'toolStatus' | 'truncated' | 'knowledge'>>,
  ) => void
  setFixProposal: (threadId: string, card: FixProposalCard) => void
  patchFixProposal: (
    threadId: string,
    proposalId: string,
    patch: Partial<FixProposalCard>,
    itemPatches?: {index: number; patch: Partial<FixProposalItem>}[],
  ) => void
  replaceStreamTools: (threadId: string, calls: ToolCallRecord[]) => void
  replaceStreamFixes: (threadId: string, cards: FixProposalCard[]) => void
  updateStream: (
    threadId: string,
    patch: Partial<Pick<StreamSlot, 'text' | 'isThinking' | 'tokens' | 'toolStatus'>>,
  ) => void
  activeStreamCount: () => number
  canStartStream: () => boolean

  createThread: (flowId: string) => string
  hydrateThreads: (flowId: string) => string[]
  switchThread: (threadId: string) => void
  lastActiveByFlow: Record<string, string>
  threadForFlow: (flowId: string) => string | null
  closeThread: (threadId: string) => void
  updateThread: (threadId: string, patch: Partial<ChatThread>) => void
  setContextBlock: (threadId: string, blockId: string | null, doc: FlowDocument | null) => void
  remapContextBlocks: (prev: FlowDocument | null, next: FlowDocument) => void
  setThreadModel: (threadId: string, pin: {provider: string; model: string; modelLabel?: string} | null) => void
  getActiveThread: () => ChatThread | null
  getFlowThreads: (flowId: string) => ChatThread[]
  clearFlowThreads: (flowId: string) => void

  bumpProviderEpoch: () => void
  setStagedPrompt: (p: {threadId: string; text: string} | null) => void
  setDraft: (threadId: string, text: string) => void
}
