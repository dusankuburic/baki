import type {StateCreator} from 'zustand'
import type {ChatMessage} from '@/types'
import type {ChatState} from './state'

// historyTokenSeq generates the per-load generation tokens (see
// HistoryStatus): a load that resolves after the thread was cleared,
// reloaded or closed carries a stale token and is dropped. Bumped by the
// history slice below and by the message/thread slices that make a thread's
// local state authoritative (clearThreadMessages, createThread).
let historyTokenSeq = 0

export function nextHistoryToken(): number {
  return ++historyTokenSeq
}

// mergeHistory lays the server's copy under the messages on screen: loaded
// messages in their stored order, and each local-only message (an error
// bubble, a turn sent while the load was in flight) right after the loaded
// message that precedes it locally — not all shoved to the end, where an old
// error bubble became the "last reply" Retry acted on. Local-only messages
// with no loaded predecessor were sent while the load ran and are newer than
// everything stored, so they trail it.
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
