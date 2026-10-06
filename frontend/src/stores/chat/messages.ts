import type {StateCreator} from 'zustand'
import type {ChatMessage} from '@/types'
import type {ChatState} from './state'
import {nextHistoryToken} from './history'

const EMPTY_ARRAY: readonly ChatMessage[] = Object.freeze([] as ChatMessage[])

type MessagesSlice = Pick<
  ChatState,
  'getMessages' | 'appendMessage' | 'removeMessage' | 'clearThreadMessages' | 'setThreadMessages' | 'compactThread'
>

export const createMessagesSlice: StateCreator<ChatState, [], [], MessagesSlice> = (set, get) => ({
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

  // Clearing makes the (now empty) local thread authoritative: an empty list
  // is kept rather than deleted, and the history status moves to 'loaded' with
  // a fresh token so a load still in flight cannot resurrect the old turns.
  clearThreadMessages: threadId =>
    set(state => {
      // A thread closed while its clear request was in flight stays gone.
      if (!state.threads.some(t => t.id === threadId)) return state
      const next = new Map(state.conversations)
      next.set(threadId, [])
      return {
        conversations: next,
        historyStatus: {...state.historyStatus, [threadId]: {state: 'loaded', token: nextHistoryToken()}},
      }
    }),

  setThreadMessages: (threadId, messages) =>
    set(state => {
      if (!state.threads.some(t => t.id === threadId)) return state
      const next = new Map(state.conversations)
      next.set(threadId, [...messages])
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
})
