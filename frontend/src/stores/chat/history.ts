import type {StateCreator} from 'zustand'
import type {ChatMessage} from '@/types'
import type {ChatState} from './state'
import type {HistoryStatus} from './types'

let historyTokenSeq = 0

export function nextHistoryToken(): number {
  return ++historyTokenSeq
}

/**
 * The loaded (server) list merged with the messages only this client has. Server order wins; a local-only message
 * follows the message it followed locally. Local messages before the first one the server still holds are turns the
 * server trimmed (it keeps the newest 200 / 1 MiB): they stay in front, where they were, and are never moved behind
 * the server's list, which would make them the newest. With no message in common at all (typed while the history
 * loaded) the local messages come after the loaded ones.
 */
export function mergeHistory(loaded: ChatMessage[], local: readonly ChatMessage[]): ChatMessage[] {
  const loadedIds = new Set(loaded.map(m => m.id))
  const anchored = local.some(m => loadedIds.has(m.id))
  const head: ChatMessage[] = []
  const after = new Map<string, ChatMessage[]>()
  const trailing: ChatMessage[] = []
  let anchor: string | null = null
  for (const m of local) {
    if (loadedIds.has(m.id)) {
      anchor = m.id
      continue
    }
    if (anchor === null) {
      ;(anchored ? head : trailing).push(m)
      continue
    }
    const list = after.get(anchor) ?? []
    list.push(m)
    after.set(anchor, list)
  }
  const merged: ChatMessage[] = [...head]
  for (const m of loaded) {
    merged.push(m)
    const tail = after.get(m.id)
    if (tail) merged.push(...tail)
  }
  merged.push(...trailing)
  return merged
}

type HistorySlice = Pick<
  ChatState,
  | 'beginHistoryLoad'
  | 'finishHistoryLoad'
  | 'markHistoryLoaded'
  | 'isHistorySynced'
  | 'getConversationVersion'
  | 'setConversationVersion'
>

export const createHistorySlice: StateCreator<ChatState, [], [], HistorySlice> = (set, get) => ({
  beginHistoryLoad: threadId => {
    const token = nextHistoryToken()
    set(state => ({historyStatus: {...state.historyStatus, [threadId]: {state: 'loading', token}}}))
    return token
  },

  finishHistoryLoad: (threadId, token, loaded, version) => {
    const cur = get().historyStatus[threadId]
    if (!cur || cur.token !== token || !get().threads.some(t => t.id === threadId)) return false
    if (loaded === null) {
      set(state => ({historyStatus: {...state.historyStatus, [threadId]: {state: 'error', token}}}))
      return true
    }
    set(state => {
      const local = state.conversations.get(threadId) ?? []
      const next = new Map(state.conversations)
      next.set(threadId, mergeHistory(loaded, local))
      const status: HistoryStatus = version === undefined ? {state: 'loaded', token} : {state: 'loaded', token, version}
      return {conversations: next, historyStatus: {...state.historyStatus, [threadId]: status}}
    })
    return true
  },

  markHistoryLoaded: threadId =>
    set(state => {
      const version = state.historyStatus[threadId]?.version
      const status: HistoryStatus =
        version === undefined
          ? {state: 'loaded', token: nextHistoryToken()}
          : {state: 'loaded', token: nextHistoryToken(), version}
      return {historyStatus: {...state.historyStatus, [threadId]: status}}
    }),

  isHistorySynced: threadId => get().historyStatus[threadId]?.state === 'loaded',

  getConversationVersion: threadId => get().historyStatus[threadId]?.version,

  setConversationVersion: (threadId, version) =>
    set(state => {
      const cur = state.historyStatus[threadId]
      if (!cur || cur.version === version) return state
      return {historyStatus: {...state.historyStatus, [threadId]: {...cur, version}}}
    }),
})
