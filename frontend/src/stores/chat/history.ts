import type {StateCreator} from 'zustand'
import type {ChatMessage} from '@/types'
import type {ChatState} from './state'

let historyTokenSeq = 0

export function nextHistoryToken(): number {
  return ++historyTokenSeq
}

function mergeHistory(loaded: ChatMessage[], local: readonly ChatMessage[]): ChatMessage[] {
  const loadedIds = new Set(loaded.map(m => m.id))
  const after = new Map<string, ChatMessage[]>()
  const trailing: ChatMessage[] = []
  let anchor: string | null = null
  for (const m of local) {
    if (loadedIds.has(m.id)) {
      anchor = m.id
      continue
    }
    if (anchor === null) {
      trailing.push(m)
      continue
    }
    const list = after.get(anchor) ?? []
    list.push(m)
    after.set(anchor, list)
  }
  const merged: ChatMessage[] = []
  for (const m of loaded) {
    merged.push(m)
    const tail = after.get(m.id)
    if (tail) merged.push(...tail)
  }
  merged.push(...trailing)
  return merged
}

type HistorySlice = Pick<ChatState, 'beginHistoryLoad' | 'finishHistoryLoad' | 'markHistoryLoaded' | 'isHistorySynced'>

export const createHistorySlice: StateCreator<ChatState, [], [], HistorySlice> = (set, get) => ({
  beginHistoryLoad: threadId => {
    const token = nextHistoryToken()
    set(state => ({historyStatus: {...state.historyStatus, [threadId]: {state: 'loading', token}}}))
    return token
  },

  finishHistoryLoad: (threadId, token, loaded) => {
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
      return {conversations: next, historyStatus: {...state.historyStatus, [threadId]: {state: 'loaded', token}}}
    })
    return true
  },

  markHistoryLoaded: threadId =>
    set(state => ({
      historyStatus: {...state.historyStatus, [threadId]: {state: 'loaded', token: nextHistoryToken()}},
    })),

  isHistorySynced: threadId => get().historyStatus[threadId]?.state === 'loaded',
})
