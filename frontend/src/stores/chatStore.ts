import {create} from 'zustand'
import {registerStoreReset} from './storeRegistry'
import {chatApi} from '@/api'
import type {ChatMessage, ProviderID} from '@/types'

// Mirrors the backend per-caller concurrency cap in internal/service/chat.go
// (maxConcurrentStreamsPerScope). The backend is authoritative; this constant
// lets the client give immediate feedback (disable Send + tooltip) instead of
// waiting for a rejected POST. Keep the two in sync.
export const MAX_CONCURRENT_STREAMS = 3

// MAX_THREADS bounds the in-memory conversation history. Each thread holds a
// full ChatMessage[] array in the conversations Map; without a cap, a long
// session with many threads grows unbounded. When exceeded, the oldest
// inactive, non-streaming thread is evicted (its messages can be re-loaded
// from the backend on demand).
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

  getMessages: (threadId: string) => ChatMessage[]
  appendMessage: (threadId: string, message: ChatMessage) => void
  removeMessage: (threadId: string, messageId: string) => void
  clearThreadMessages: (threadId: string) => void
  compactThread: (threadId: string, keepPairs: number) => void
  updateStreamingMessage: (threadId: string, text: string) => void
  startStream: (threadId: string, streamId: string, messageId: string) => void
  endStream: (threadId: string) => void
  setStreamMeta: (threadId: string, patch: Partial<Pick<StreamSlot, 'isThinking' | 'tokens' | 'toolStatus'>>) => void
  // updateStream patches text AND meta in one atomic set, halving the per-frame
  // subscriber notifications during streaming (the high-frequency RAF-coalesced
  // flush otherwise issues two set() calls — text + tokens — at 60fps).
  updateStream: (threadId: string, patch: Partial<Pick<StreamSlot, 'text' | 'isThinking' | 'tokens' | 'toolStatus'>>) => void
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

const EMPTY_ARRAY: ChatMessage[] = []

export const useChatStore = create<ChatState>((set, get) => ({
  threads: [],
  activeThreadId: null,
  conversations: new Map(),
  streams: {},
  selectedProvider: 'claude',
  providerEpoch: 0,
  stagedPrompt: null,
  drafts: {},

  getMessages: (threadId) => {
    return get().conversations.get(threadId) ?? EMPTY_ARRAY
  },

  appendMessage: (threadId, message) => set(state => {
    const existing = state.conversations.get(threadId) ?? []
    // Dedup by message ID — prevents duplicate entries when a conversation
    // is loaded twice (e.g. rapid flow switching, StrictMode double-invoke).
    if (existing.some(m => m.id === message.id)) return state
    const next = new Map(state.conversations)
    next.set(threadId, [...existing, message])

    if (message.role === 'user' && !state.threads.find(t => t.id === threadId)?.title) {
      const title = message.content.slice(0, 40).replace(/\n/g, ' ')
      const threads = state.threads.map(t =>
        t.id === threadId ? {...t, title} : t
      )
      return {conversations: next, threads}
    }
    return {conversations: next}
  }),

  removeMessage: (threadId, messageId) => set(state => {
    const msgs = state.conversations.get(threadId)
    if (!msgs) return state
    const next = new Map(state.conversations)
    next.set(threadId, msgs.filter(m => m.id !== messageId))
    return {conversations: next}
  }),

  clearThreadMessages: (threadId) => set(state => {
    const next = new Map(state.conversations)
    next.delete(threadId)
    return {conversations: next}
  }),

  compactThread: (threadId, keepPairs) => set(state => {
    const msgs = state.conversations.get(threadId)
    if (!msgs || msgs.length <= keepPairs * 2) return state
    const next = new Map(state.conversations)
    next.set(threadId, msgs.slice(-keepPairs * 2))
    return {conversations: next}
  }),

  updateStreamingMessage: (threadId, text) => set(state => {
    const slot = state.streams[threadId]
    if (!slot) return state
    return {streams: {...state.streams, [threadId]: {...slot, text}}}
  }),

  startStream: (threadId, streamId, messageId) => set(state => ({
    streams: {
      ...state.streams,
      [threadId]: {streamId, messageId, text: '', isThinking: true, tokens: 0, toolStatus: null},
    },
  })),

  endStream: (threadId) => set(state => {
    if (!(threadId in state.streams)) return state
    const next = {...state.streams}
    delete next[threadId]
    return {streams: next}
  }),

  setStreamMeta: (threadId, patch) => set(state => {
    const slot = state.streams[threadId]
    if (!slot) return state
    return {streams: {...state.streams, [threadId]: {...slot, ...patch}}}
  }),

  updateStream: (threadId, patch) => set(state => {
    const slot = state.streams[threadId]
    if (!slot) return state
    return {streams: {...state.streams, [threadId]: {...slot, ...patch}}}
  }),

  activeStreamCount: () => Object.keys(get().streams).length,
  canStartStream: () => get().activeStreamCount() < MAX_CONCURRENT_STREAMS,

  createThread: (flowId) => {
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
        const victim = threads.find(t =>
          t.id !== id && t.id !== state.activeThreadId && !(t.id in state.streams),
        )
        if (victim) {
          threads = threads.filter(t => t.id !== victim.id)
          nextConversations.delete(victim.id)
          const drafts = victim.id in state.drafts ? {...state.drafts} : state.drafts
          if (victim.id in drafts) delete drafts[victim.id]
          return {threads, activeThreadId: id, conversations: nextConversations, drafts}
        }
      }
      return {threads: [...state.threads, thread], activeThreadId: id, conversations: nextConversations}
    })
    return id
  },

  switchThread: (threadId) => {
    const thread = get().threads.find(t => t.id === threadId)
    if (thread) set({activeThreadId: threadId})
  },

  closeThread: (threadId) => {
    // Cancel any in-flight backend stream before clearing the local slot.
    // Without this, the provider keeps generating tokens for a stream whose
    // client-side listener was deleted — wasting spend and orphaning the
    // assistant's response (the done event finds no slot and is silently
    // dropped instead of being committed to history).
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
      const activeThreadId = state.activeThreadId === threadId
        ? (remaining.length > 0 ? remaining[remaining.length - 1].id : null)
        : state.activeThreadId
      return {threads: remaining, conversations: next, activeThreadId, streams, drafts}
    })
  },

  updateThread: (threadId, patch) => set(state => ({
    threads: state.threads.map(t => t.id === threadId ? {...t, ...patch} : t),
  })),

  getActiveThread: () => {
    const {threads, activeThreadId} = get()
    return threads.find(t => t.id === activeThreadId) ?? null
  },

  getFlowThreads: (flowId) => {
    return get().threads.filter(t => t.flowId === flowId)
  },

  clearFlowThreads: (flowId) => set(state => {
    const toRemove = state.threads.filter(t => t.flowId === flowId).map(t => t.id)
    const remaining = state.threads.filter(t => t.flowId !== flowId)
    const next = new Map(state.conversations)
    for (const id of toRemove) next.delete(id)
    const activeThreadId = toRemove.includes(state.activeThreadId ?? '')
      ? (remaining.length > 0 ? remaining[remaining.length - 1].id : null)
      : state.activeThreadId
    return {threads: remaining, conversations: next, activeThreadId}
  }),

  setProvider: (p) => set({selectedProvider: p}),

  bumpProviderEpoch: () => set(s => ({providerEpoch: s.providerEpoch + 1})),
  setStagedPrompt: (p) => set({stagedPrompt: p}),

  setDraft: (threadId, text) => set(state => {
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
registerStoreReset(() => useChatStore.setState({
  threads: [], activeThreadId: null, conversations: new Map(), streams: {},
  stagedPrompt: null, selectedProvider: 'claude', providerEpoch: 0, drafts: {},
}))
