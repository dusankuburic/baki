import {create} from 'zustand'
import type {ChatMessage, ProviderID} from '@/types/domain'

export interface ChatThread {
  id: string
  flowId: string
  title: string
  createdAt: string
  contextBlockId: string | null
  selectedSourceFiles: string[]
  tokensIn: number
  tokensOut: number
}

interface ChatState {
  threads: ChatThread[]
  activeThreadId: string | null
  conversations: Map<string, ChatMessage[]>
  activeStreamId: string | null
  streamingMessageId: string | null
  streamingText: string
  selectedProvider: ProviderID
  pendingMessage: {text: string; contextBlockId?: string} | null

  getMessages: (threadId: string) => ChatMessage[]
  appendMessage: (threadId: string, message: ChatMessage) => void
  removeMessage: (threadId: string, messageId: string) => void
  clearThreadMessages: (threadId: string) => void
  compactThread: (threadId: string, keepPairs: number) => void
  updateStreamingMessage: (text: string) => void
  startStream: (streamId: string, messageId: string) => void
  endStream: () => void

  createThread: (flowId: string) => string
  switchThread: (threadId: string) => void
  closeThread: (threadId: string) => void
  updateThread: (threadId: string, patch: Partial<ChatThread>) => void
  getActiveThread: () => ChatThread | null
  getFlowThreads: (flowId: string) => ChatThread[]
  clearFlowThreads: (flowId: string) => void

  setProvider: (p: ProviderID) => void
  setPendingMessage: (p: {text: string; contextBlockId?: string} | null) => void
}

const EMPTY_ARRAY: ChatMessage[] = []

export const useChatStore = create<ChatState>((set, get) => ({
  threads: [],
  activeThreadId: null,
  conversations: new Map(),
  activeStreamId: null,
  streamingMessageId: null,
  streamingText: '',
  selectedProvider: 'claude',
  pendingMessage: null,

  getMessages: (threadId) => {
    return get().conversations.get(threadId) ?? EMPTY_ARRAY
  },

  appendMessage: (threadId, message) => set(state => {
    const next = new Map(state.conversations)
    next.set(threadId, [...(next.get(threadId) ?? []), message])

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

  updateStreamingMessage: (text) => set({streamingText: text}),

  startStream: (streamId, messageId) => set({
    activeStreamId: streamId,
    streamingMessageId: messageId,
    streamingText: '',
  }),

  endStream: () => set({
    activeStreamId: null,
    streamingMessageId: null,
    streamingText: '',
  }),

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
    set(state => ({
      threads: [...state.threads, thread],
      activeThreadId: id,
    }))
    return id
  },

  switchThread: (threadId) => {
    const thread = get().threads.find(t => t.id === threadId)
    if (thread) set({activeThreadId: threadId})
  },

  closeThread: (threadId) => set(state => {
    const remaining = state.threads.filter(t => t.id !== threadId)
    const next = new Map(state.conversations)
    next.delete(threadId)
    const activeThreadId = state.activeThreadId === threadId
      ? (remaining.length > 0 ? remaining[remaining.length - 1].id : null)
      : state.activeThreadId
    return {threads: remaining, conversations: next, activeThreadId}
  }),

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
  setPendingMessage: (p) => set({pendingMessage: p}),
}))
