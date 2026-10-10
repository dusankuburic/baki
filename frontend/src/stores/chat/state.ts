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

// ChatState is the whole chat store's state shape: the data fields are laid
// out here, and each group of actions below is implemented by one slice in
// this directory (history.ts, messages.ts, streams.ts, threads.ts,
// settings.ts) — store.ts composes the slices into the single store.
export interface ChatState {
  threads: ChatThread[]
  activeThreadId: string | null
  conversations: Map<string, ChatMessage[]>
  // streams maps threadId → its in-flight response; absent = not streaming.
  streams: Record<string, StreamSlot>
  // The provider for NEW chats, mirrored from settings' ai.activeProvider by
  // the Settings panels and the start-up restore. The chat itself no longer
  // reads it: each thread resolves its own pair (its pin, else the active
  // provider's "Default for new chats" — lib/modelSelection), and a fallback
  // never writes it.
  selectedProvider: ProviderID
  providerEpoch: number
  // modelPickerRequest bumps to ask the chat header's model picker to open
  // (a notice's "Choose model", an error bubble's "Choose another model");
  // the picker opens on a change it sees while mounted.
  modelPickerRequest: number
  openModelPicker: () => void
  // modelRefusals: per thread, the model the server last refused a send for
  // (see ModelRefusal). Cleared by the thread's next accepted send, a pick,
  // dismissing its notice, or closing it.
  modelRefusals: Record<string, ModelRefusal>
  setModelRefusal: (threadId: string, refusal: ModelRefusal | null) => void
  // stagedPrompt carries a grounded prompt (from "Explain/Fix with AI") into a
  // thread's composer for review before the user sends it — nothing auto-sends.
  // Consumed by ChatInput once its threadId is the active thread, then cleared.
  stagedPrompt: {threadId: string; text: string} | null
  // drafts holds each thread's unsent composer text, keyed by threadId, so a
  // half-typed message survives switching threads or closing/reopening the AI
  // tab. Session-only (never persisted to the backend conversation payload).
  drafts: Record<string, string>
  // historyStatus: per-thread server-history load state (see HistoryState).
  // Absent = never attempted (a thread hydrated from localStorage).
  historyStatus: Record<string, HistoryStatus>
  // beginHistoryLoad marks the thread 'loading' and returns the load's token.
  beginHistoryLoad: (threadId: string) => number
  // finishHistoryLoad applies a load result if its token is still current:
  // loaded messages are MERGED BEFORE anything the user sent meanwhile
  // (deduped by id); `null` records a failed load. Returns whether applied.
  finishHistoryLoad: (threadId: string, token: number, loaded: ChatMessage[] | null) => boolean
  // markHistoryLoaded declares the thread's local messages authoritative
  // (a new thread, or one just cleared) and invalidates any load in flight.
  markHistoryLoaded: (threadId: string) => void
  isHistorySynced: (threadId: string) => boolean

  getMessages: (threadId: string) => readonly ChatMessage[]
  appendMessage: (threadId: string, message: ChatMessage) => void
  removeMessage: (threadId: string, messageId: string) => void
  clearThreadMessages: (threadId: string) => void
  // setThreadMessages replaces a thread's messages wholesale — Compact applies
  // the exact list it just persisted, so screen and server agree.
  setThreadMessages: (threadId: string, messages: readonly ChatMessage[]) => void
  compactThread: (threadId: string, keepPairs: number) => void
  updateStreamingMessage: (threadId: string, text: string) => void
  startStream: (threadId: string, streamId: string, messageId: string) => void
  // Queued follow-up (U1.6): one per thread, sent automatically when the
  // thread's stream ends. Composing while streaming is the expected UX — the
  // queue replaces a dead textarea.
  queuedByThread: Record<string, QueuedMessage>
  queueMessage: (threadId: string, msg: QueuedMessage) => void
  clearQueuedMessage: (threadId: string) => void
  takeQueuedMessage: (threadId: string) => QueuedMessage | undefined
  // addToolCall appends a finished tool execution (tool_result event) to the
  // thread's live stream slot.
  addToolCall: (threadId: string, record: ToolCallRecord) => void
  endStream: (threadId: string) => void
  setStreamMeta: (threadId: string, patch: Partial<Pick<StreamSlot, 'isThinking' | 'tokens' | 'toolStatus'>>) => void
  // setFixProposal shows one approval card on the thread's stream slot —
  // appended unless a card with the same proposalId exists (journal replay is
  // idempotent). patchFixProposal merges a decision/outcome update into the
  // matching card (+ per-item patches for batches).
  setFixProposal: (threadId: string, card: FixProposalCard) => void
  patchFixProposal: (
    threadId: string,
    proposalId: string,
    patch: Partial<FixProposalCard>,
    itemPatches?: {index: number; patch: Partial<FixProposalItem>}[],
  ) => void
  // replaceStreamTools / replaceStreamFixes wholesale-replace the slot's tool
  // trail / proposal cards from an authoritative journal replay on reconnect —
  // replace (not append) keeps double-replay idempotent.
  replaceStreamTools: (threadId: string, calls: ToolCallRecord[]) => void
  replaceStreamFixes: (threadId: string, cards: FixProposalCard[]) => void
  // updateStream patches text AND meta in one atomic set, halving the per-frame
  // subscriber notifications during streaming (the high-frequency RAF-coalesced
  // flush otherwise issues two set() calls — text + tokens — at 60fps).
  updateStream: (
    threadId: string,
    patch: Partial<Pick<StreamSlot, 'text' | 'isThinking' | 'tokens' | 'toolStatus'>>,
  ) => void
  // activeStreamCount is the number of threads currently generating. Used by
  // the client-side cap guard (see MAX_CONCURRENT_STREAMS).
  activeStreamCount: () => number
  canStartStream: () => boolean

  createThread: (flowId: string) => string
  // hydrateThreads restores a flow's persisted thread list (see
  // persistFlowThreads). Returns the flow's thread ids — the ones already in
  // the store when present, otherwise the restored ones (last one active),
  // otherwise []. Conversations are not loaded here; the active-thread effect
  // fetches each thread's history by its key on demand.
  hydrateThreads: (flowId: string) => string[]
  switchThread: (threadId: string) => void
  // lastActiveByFlow remembers each flow's most recently active thread
  // (session-only), so a flow that is re-opened or refreshed returns to the
  // thread the user was in rather than merely the newest one.
  lastActiveByFlow: Record<string, string>
  // threadForFlow picks the thread to show for a flow: its last active thread
  // if still open, else its newest thread, else null.
  threadForFlow: (flowId: string) => string | null
  closeThread: (threadId: string) => void
  updateThread: (threadId: string, patch: Partial<ChatThread>) => void
  // setThreadModel pins a thread to a model (null unpins it) and persists it
  // with the thread list. A no-op when the pin is unchanged, and for an
  // incomplete pin or an unknown provider id. A label that is only an id is
  // not kept.
  setThreadModel: (threadId: string, pin: {provider: string; model: string; modelLabel?: string} | null) => void
  getActiveThread: () => ChatThread | null
  getFlowThreads: (flowId: string) => ChatThread[]
  clearFlowThreads: (flowId: string) => void

  setProvider: (p: ProviderID) => void
  bumpProviderEpoch: () => void
  setStagedPrompt: (p: {threadId: string; text: string} | null) => void
  setDraft: (threadId: string, text: string) => void
}
