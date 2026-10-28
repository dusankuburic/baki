import type {StateCreator} from 'zustand'
import type {ChatMessage} from '@/types'
import type {ChatState} from './state'
import {nextHistoryToken} from './history'
import {autoTitle} from './autoTitle'

const EMPTY_ARRAY: readonly ChatMessage[] = Object.freeze([] as ChatMessage[])

type MessagesSlice = Pick<
  ChatState,
  | 'getMessages'
  | 'appendMessage'
  | 'removeMessage'
  | 'updateMessage'
  | 'clearThreadMessages'
  | 'setThreadMessages'
  | 'compactThread'
>

export const createMessagesSlice: StateCreator<ChatState, [], [], MessagesSlice> = (set, get) => ({
  getMessages: threadId => {
    const msgs = get().conversations.get(threadId)
    return msgs ? [...msgs] : EMPTY_ARRAY
  },

  appendMessage: (threadId, message) =>
    set(state => {
      const existing = state.conversations.get(threadId) ?? []
      if (existing.some(m => m.id === message.id)) return state
      const next = new Map(state.conversations)
      next.set(threadId, [...existing, message])

      const title = message.role === 'user' ? autoTitle(message.content) : ''
      if (title && !state.threads.find(t => t.id === threadId)?.title) {
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

  updateMessage: (threadId, messageId, patch) =>
    set(state => {
      const msgs = state.conversations.get(threadId)
      const i = msgs?.findIndex(m => m.id === messageId) ?? -1
      if (!msgs || i < 0) return state
      const next = new Map(state.conversations)
      next.set(
        threadId,
        msgs.map((m, j) => (j === i ? patch(m) : m)),
      )
      return {conversations: next}
    }),

  clearThreadMessages: threadId =>
    set(state => {
      if (!state.threads.some(t => t.id === threadId)) return state
      const next = new Map(state.conversations)
      next.set(threadId, [])
      return {
        conversations: next,
        // The server has no conversation after a clear, whose version is 0.
        historyStatus: {...state.historyStatus, [threadId]: {state: 'loaded', token: nextHistoryToken(), version: 0}},
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
