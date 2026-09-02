import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import {chatApi} from '@/api'
import type {ChatMessage, ProviderID, ToolCallRecord} from '@/types'

// Mirrors the backend per-caller concurrency cap in internal/service/chat.go
// (maxConcurrentStreamsPerScope). The backend is authoritative; this constant
// lets the client give immediate feedback (disable Send + tooltip) instead of
// waiting for a rejected POST. Keep the two in sync.
export const MAX_CONCURRENT_STREAMS = 3

// MAX_THREADS bounds the in-memory conversation history; when exceeded, the
// oldest inactive, non-streaming thread is evicted (messages re-load from the
// backend on demand).
export const MAX_THREADS = 50

export interface ChatThread {
  id: string
  flowId: string
  title: string
  createdAt: string
  contextBlockId: string | null
  selectedSourceFiles: string[]
  tokensIn: number
  tokensOut: number
  // useTools opts this thread into the read-only tool/agent loop (default off).
  useTools?: boolean
  // usedTools/appliedFixes are set at commit when the stream actually ran
  // tools / landed an applied fix — thread-tab badges so a glance shows which
  // conversations were agentic.
  usedTools?: boolean
  appliedFixes?: boolean
}

// FixProposalItem is one fix inside a (possibly batch) approval prompt, with
// its own resolved outcome after the batch decision lands.
export interface FixProposalItem {
  ruleId: string
  fixType: string
  blockLabel: string
  line: number
  summary: string
  // pending → applied | applied-unresolved | error | already-resolved.
  status: 'pending' | 'applied' | 'applied-unresolved' | 'error' | 'already-resolved'
  message?: string
}

// FixProposalCard is one approval prompt on a thread's stream slot (single or
// batch). Lives on the StreamSlot (transient by design): cards appear when
// the model requests fix(es), resolve when the user decides / the backend
// reports the outcome, and disappear with the slot when the stream ends —
// the committed message carries the persisted snapshots.
export interface FixProposalCard {
  proposalId: string
  // pending → applying (approved, mutation in flight) → applied |
  // applied-unresolved | declined | timeout | error. 'pending' is the only
  // state with enabled buttons.
  status: 'pending' | 'applying' | 'applied' | 'applied-unresolved' | 'declined' | 'timeout' | 'error'
  message?: string
  // items carries per-fix state; single-fix prompts have exactly one item and
  // ALSO mirror it on the flat fields below (rendering + legacy persistence).
  items: FixProposalItem[]
  // Flat mirrors of items[0] for single-fix prompts (empty for pure batches).
  ruleId: string
  fixType: string
  blockLabel: string
  line: number
  summary: string
}

// StreamSlot is one thread's in-flight AI response. Streams are per-thread so
// several threads can generate in parallel; the thinking/tokens/tool state
// lives here (not in hook-local state) so a background thread's progress
// survives switching tabs away and back.
export interface StreamSlot {
  streamId: string
  messageId: string
  text: string
  isThinking: boolean
  tokens: number
  toolStatus: string | null
  // toolCalls accumulates the finished tool executions (tool_result events)
  // for the response being streamed; committed onto the assistant message
  // when the stream ends.
  toolCalls: ToolCallRecord[]
  // fixProposals stacks this stream's approval prompts: a stream can carry
  // several sequential proposals (fix → continue → next fix), and each batch
  // proposal is one card with per-item rows.
  fixProposals: FixProposalCard[]
}

interface ChatState {
  threads: ChatThread[]
  activeThreadId: string | null
  conversations: Map<string, ChatMessage[]>
  // streams maps threadId → its in-flight response; absent = not streaming.
  streams: Record<string, StreamSlot>
  selectedProvider: ProviderID
  providerEpoch: number
  // stagedPrompt carries a grounded prompt (from "Explain/Fix with AI") into a
  // thread's composer for review before the user sends it — nothing auto-sends.
  // Consumed by ChatInput once its threadId is the active thread, then cleared.
  stagedPrompt: {threadId: string; text: string} | null
  // drafts holds each thread's unsent composer text, keyed by threadId, so a
  // half-typed message survives switching threads or closing/reopening the AI
  // tab. Session-only (never persisted to the backend conversation payload).
  drafts: Record<string, string>

  getMessages: (threadId: string) => readonly ChatMessage[]
  appendMessage: (threadId: string, message: ChatMessage) => void
  removeMessage: (threadId: string, messageId: string) => void
  clearThreadMessages: (threadId: string) => void
  compactThread: (threadId: string, keepPairs: number) => void
  updateStreamingMessage: (threadId: string, text: string) => void
  startStream: (threadId: string, streamId: string, messageId: string) => void
  // Queued follow-up (U1.6): one per thread, sent automatically when the
  // thread's stream ends. Composing while streaming is the expected UX — the
  // queue replaces a dead textarea.
  queuedByThread: Record<string, {text: string; files: string[]; excludeContext?: boolean}>
  queueMessage: (threadId: string, msg: {text: string; files: string[]; excludeContext?: boolean}) => void
  clearQueuedMessage: (threadId: string) => void
  takeQueuedMessage: (threadId: string) => {text: string; files: string[]; excludeContext?: boolean} | undefined
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
  patchFixProposal: (threadId: string, proposalId: string, patch: Partial<FixProposalCard>, itemPatches?: {ruleId: string; patch: Partial<FixProposalItem>}[]) => void
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
  switchThread: (threadId: string) => void
  closeThread: (threadId: string) => void
  updateThread: (threadId: string, patch: Partial<ChatThread>) => void
  getActiveThread: () => ChatThread | null
  getFlowThreads: (flowId: string) => ChatThread[]
  clearFlowThreads: (flowId: string) => void

  setProvider: (p: ProviderID) => void
  bumpProviderEpoch: () => void
  setStagedPrompt: (p: {threadId: string; text: string} | null) => void
  setDraft: (threadId: string, text: string) => void
}

const EMPTY_ARRAY: readonly ChatMessage[] = Object.freeze([] as ChatMessage[])

export const useChatStore = create<ChatState>((set, get) => ({
  threads: [],
  activeThreadId: null,
  conversations: new Map(),
  streams: {},
  queuedByThread: {},
  selectedProvider: 'claude',
  providerEpoch: 0,
  stagedPrompt: null,
  drafts: {},

  getMessages: threadId => {
    // Defensive copy: without this, callers receive the live internal array
    // reference and an accidental in-place mutation (push/splice) would
    // corrupt the store's state without triggering reactivity. EMPTY_ARRAY
    // is already immutable so it's returned as-is for missing threads.
    const msgs = get().conversations.get(threadId)
    return msgs ? [...msgs] : EMPTY_ARRAY
  },

  appendMessage: (threadId, message) =>
    set(state => {
      const existing = state.conversations.get(threadId) ?? []
      // Dedup by message ID — prevents duplicate entries when a conversation
      // is loaded twice (e.g. rapid flow switching, StrictMode double-invoke).
      if (existing.some(m => m.id === message.id)) return state
      const next = new Map(state.conversations)
      next.set(threadId, [...existing, message])

      if (message.role === 'user' && !state.threads.find(t => t.id === threadId)?.title) {
        const title = message.content.slice(0, 40).replace(/\n/g, ' ')
        const threads = state.threads.map(t => (t.id === threadId ? {...t, title} : t))
        return {conversations: next, threads}
      }
      return {conversations: next}
    }),

  removeMessage: (threadId, messageId) =>
    set(state => {
      const msgs = state.conversations.get(threadId)
      if (!msgs) return state
      const next = new Map(state.conversations)
      next.set(
        threadId,
        msgs.filter(m => m.id !== messageId),
      )
      return {conversations: next}
    }),

  clearThreadMessages: threadId =>
    set(state => {
      const next = new Map(state.conversations)
      next.delete(threadId)
      return {conversations: next}
    }),

  compactThread: (threadId, keepPairs) =>
    set(state => {
      const msgs = state.conversations.get(threadId)
      if (!msgs || msgs.length <= keepPairs * 2) return state
      const next = new Map(state.conversations)
      next.set(threadId, msgs.slice(-keepPairs * 2))
      return {conversations: next}
    }),

  updateStreamingMessage: (threadId, text) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, text}}}
    }),

  startStream: (threadId, streamId, messageId) =>
    set(state => ({
      streams: {
        ...state.streams,
        [threadId]: {streamId, messageId, text: '', isThinking: true, tokens: 0, toolStatus: null, toolCalls: [], fixProposals: []},
      },
    })),

  // addToolCall appends one finished tool execution to the thread's stream
  // slot (the live view of the per-message tool trail).
  addToolCall: (threadId, record) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, toolCalls: [...slot.toolCalls, record]}}}
    }),

  replaceStreamTools: (threadId, calls) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, toolCalls: calls}}}
    }),

  replaceStreamFixes: (threadId, cards) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, fixProposals: cards}}}
    }),

  endStream: threadId =>
    set(state => {
      if (!(threadId in state.streams)) return state
      const next = {...state.streams}
      delete next[threadId]
      return {streams: next}
    }),

  queueMessage: (threadId, msg) => set(state => ({queuedByThread: {...state.queuedByThread, [threadId]: msg}})),
  clearQueuedMessage: threadId =>
    set(state => {
      if (!(threadId in state.queuedByThread)) return state
      const next = {...state.queuedByThread}
      delete next[threadId]
      return {queuedByThread: next}
    }),
  takeQueuedMessage: threadId => {
    const msg = get().queuedByThread[threadId]
    if (msg) get().clearQueuedMessage(threadId)
    return msg
  },

  // setFixProposal appends a card unless one with the same proposalId already
  // exists (replay idempotence) — sequential proposals stack, earlier
  // outcomes are kept.
  setFixProposal: (threadId, card) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      if (slot.fixProposals.some(c => c.proposalId === card.proposalId)) return state
      return {streams: {...state.streams, [threadId]: {...slot, fixProposals: [...slot.fixProposals, card]}}}
    }),

  // patchFixProposal merges a status/message update into the matching card
  // (a no-op when the card is gone — the stream may have ended) and applies
  // per-item patches (batch decisions) by ruleId.
  patchFixProposal: (threadId, proposalId, patch, itemPatches) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      const idx = slot.fixProposals.findIndex(c => c.proposalId === proposalId)
      if (idx < 0) return state
      const card = slot.fixProposals[idx]
      let items = card.items
      if (itemPatches && itemPatches.length > 0) {
        items = card.items.map(it => {
          const p = itemPatches.find(ip => ip.ruleId === it.ruleId)
          return p ? {...it, ...p.patch} : it
        })
      }
      const fixProposals = [...slot.fixProposals]
      fixProposals[idx] = {...card, ...patch, items}
      return {streams: {...state.streams, [threadId]: {...slot, fixProposals}}}
    }),

  setStreamMeta: (threadId, patch) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, ...patch}}}
    }),

  updateStream: (threadId, patch) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      return {streams: {...state.streams, [threadId]: {...slot, ...patch}}}
    }),

  activeStreamCount: () => Object.keys(get().streams).length,
  canStartStream: () => get().activeStreamCount() < MAX_CONCURRENT_STREAMS,

  createThread: flowId => {
    const id = crypto.randomUUID()
    const thread: ChatThread = {
      id,
      flowId,
      title: '',
      createdAt: new Date().toISOString(),
      contextBlockId: null,
      selectedSourceFiles: [],
      tokensIn: 0,
      tokensOut: 0,
    }
    set(state => {
      // Pre-populate with [] so the backend-restore effect treats this as
      // already initialised. Without this, every new thread would re-load
      // the previous thread's conversation from disk (same 'flow' scope).
      const nextConversations = new Map(state.conversations)
      nextConversations.set(id, [])
      let threads = [...state.threads, thread]
      // Evict the oldest inactive, non-streaming thread when the cap is
      // exceeded, so long sessions don't accumulate unbounded chat history.
      if (threads.length > MAX_THREADS) {
        const victim = threads.find(t => t.id !== id && t.id !== state.activeThreadId && !(t.id in state.streams))
        if (victim) {
          threads = threads.filter(t => t.id !== victim.id)
          nextConversations.delete(victim.id)
          const drafts = victim.id in state.drafts ? {...state.drafts} : state.drafts
          if (victim.id in drafts) delete drafts[victim.id]
          const queuedByThread = victim.id in state.queuedByThread ? {...state.queuedByThread} : state.queuedByThread
          if (victim.id in queuedByThread) delete queuedByThread[victim.id]
          return {threads, activeThreadId: id, conversations: nextConversations, drafts, queuedByThread}
        }
      }
      return {threads: [...state.threads, thread], activeThreadId: id, conversations: nextConversations}
    })
    return id
  },

  switchThread: threadId => {
    const thread = get().threads.find(t => t.id === threadId)
    if (thread) set({activeThreadId: threadId})
  },

  closeThread: threadId => {
    // Cancel any in-flight backend stream before clearing the local slot, or
    // the provider keeps generating for a stream with no listener.
    const slot = get().streams[threadId]
    if (slot?.streamId) {
      chatApi.cancelStream(slot.streamId).catch(() => {})
    }
    set(state => {
      const remaining = state.threads.filter(t => t.id !== threadId)
      const next = new Map(state.conversations)
      next.delete(threadId)
      let streams = state.streams
      if (threadId in streams) {
        streams = {...streams}
        delete streams[threadId]
      }
      const drafts = threadId in state.drafts ? {...state.drafts} : state.drafts
      if (threadId in drafts) delete drafts[threadId]
      const activeThreadId =
        state.activeThreadId === threadId
          ? remaining.length > 0
            ? remaining[remaining.length - 1].id
            : null
          : state.activeThreadId
      return {threads: remaining, conversations: next, activeThreadId, streams, drafts}
    })
  },

  updateThread: (threadId, patch) =>
    set(state => ({
      threads: state.threads.map(t => (t.id === threadId ? {...t, ...patch} : t)),
    })),

  getActiveThread: () => {
    const {threads, activeThreadId} = get()
    return threads.find(t => t.id === activeThreadId) ?? null
  },

  getFlowThreads: flowId => {
    return get().threads.filter(t => t.flowId === flowId)
  },

  clearFlowThreads: flowId =>
    set(state => {
      const toRemove = state.threads.filter(t => t.flowId === flowId).map(t => t.id)
      const remaining = state.threads.filter(t => t.flowId !== flowId)
      const next = new Map(state.conversations)
      for (const id of toRemove) next.delete(id)
      const activeThreadId = toRemove.includes(state.activeThreadId ?? '')
        ? remaining.length > 0
          ? remaining[remaining.length - 1].id
          : null
        : state.activeThreadId
      // Full per-thread cleanup (F1.7), mirroring closeThread: streams,
      // drafts, and queued follow-ups for removed threads must not linger
      // (a lingering stream would eat a concurrency-cap slot forever).
      const streams = {...state.streams}
      const drafts = {...state.drafts}
      const queuedByThread = {...state.queuedByThread}
      for (const id of toRemove) {
        delete streams[id]
        delete drafts[id]
        delete queuedByThread[id]
      }
      return {threads: remaining, conversations: next, activeThreadId, streams, drafts, queuedByThread}
    }),

  setProvider: p => set({selectedProvider: p}),

  bumpProviderEpoch: () => set(s => ({providerEpoch: s.providerEpoch + 1})),
  setStagedPrompt: p => set({stagedPrompt: p}),

  setDraft: (threadId, text) =>
    set(state => {
      // Prune empty drafts so the map doesn't accumulate blank keys.
      if (!text) {
        if (!(threadId in state.drafts)) return state
        const drafts = {...state.drafts}
        delete drafts[threadId]
        return {drafts}
      }
      if (state.drafts[threadId] === text) return state
      return {drafts: {...state.drafts, [threadId]: text}}
    }),
}))

// Reset on logout (see storeRegistry).
registerStoreReset(() =>
  useChatStore.setState({
    threads: [],
    activeThreadId: null,
    conversations: new Map(),
    streams: {},
    stagedPrompt: null,
    selectedProvider: 'claude',
    providerEpoch: 0,
    drafts: {},
    // Queued (unsent) follow-ups are session state — they must not survive
    // logout (F1.7).
    queuedByThread: {},
  }),
)
