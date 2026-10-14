import type {ChatMessage, ProviderID, ToolCallRecord} from '@/types'
import type {
  ChatThread,
  FixProposalCard,
  FixProposalItem,
  HistoryStatus,
  ModelRefusal,
  QueuedMessage,
  StreamSlot,
} from './types'

export interface ChatState {
  threads: ChatThread[]
  activeThreadId: string | null
  conversations: Map<string, ChatMessage[]>
  streams: Record<string, StreamSlot>
  selectedProvider: ProviderID
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
  clearThreadMessages: (threadId: string) => void
  setThreadMessages: (threadId: string, messages: readonly ChatMessage[]) => void
  compactThread: (threadId: string, keepPairs: number) => void
  updateStreamingMessage: (threadId: string, text: string) => void
  startStream: (threadId: string, streamId: string, messageId: string) => void
  queuedByThread: Record<string, QueuedMessage>
  queueMessage: (threadId: string, msg: QueuedMessage) => void
  clearQueuedMessage: (threadId: string) => void
  takeQueuedMessage: (threadId: string) => QueuedMessage | undefined
  addToolCall: (threadId: string, record: ToolCallRecord) => void
  endStream: (threadId: string) => void
  setStreamMeta: (threadId: string, patch: Partial<Pick<StreamSlot, 'isThinking' | 'tokens' | 'toolStatus'>>) => void
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
  setThreadModel: (threadId: string, pin: {provider: string; model: string; modelLabel?: string} | null) => void
  getActiveThread: () => ChatThread | null
  getFlowThreads: (flowId: string) => ChatThread[]
  clearFlowThreads: (flowId: string) => void

  setProvider: (p: ProviderID) => void
  bumpProviderEpoch: () => void
  setStagedPrompt: (p: {threadId: string; text: string} | null) => void
  setDraft: (threadId: string, text: string) => void
}
