import type {StateCreator} from 'zustand'
import {chatApi} from '@/api'
import {uuid} from '@/lib/uuid'
import {storableModelLabel} from '@/lib/modelIds'
import type {ChatState} from './state'
import {MAX_THREADS} from './types'
import type {ChatThread, HistoryStatus, ThreadPinFields} from './types'
import {nextHistoryToken} from './history'
import {persistFlowThreads, readStoredThreads} from './persistence'

function nextThreadOfFlow(threads: readonly ChatThread[], flowId: string | undefined): string | null {
  if (!flowId) return null
  for (let i = threads.length - 1; i >= 0; i--) {
    const t = threads[i]
    if (t && t.flowId === flowId) return t.id
  }
  return null
}

type ThreadSlice = Pick<
  ChatState,
  | 'createThread'
  | 'hydrateThreads'
  | 'switchThread'
  | 'threadForFlow'
  | 'closeThread'
  | 'updateThread'
  | 'setThreadModel'
  | 'getActiveThread'
  | 'getFlowThreads'
  | 'clearFlowThreads'
>

export const createThreadSlice: StateCreator<ChatState, [], [], ThreadSlice> = (set, get) => ({
  createThread: flowId => {
    const id = uuid()
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
      const nextConversations = new Map(state.conversations)
      nextConversations.set(id, [])
      const historyStatus: Record<string, HistoryStatus> = {
        ...state.historyStatus,
        [id]: {state: 'loaded', token: nextHistoryToken()},
      }
      let threads = [...state.threads, thread]
      if (threads.length > MAX_THREADS) {
        const victim = threads.find(t => t.id !== id && t.id !== state.activeThreadId && !(t.id in state.streams))
        if (victim) {
          threads = threads.filter(t => t.id !== victim.id)
          nextConversations.delete(victim.id)
          const drafts = victim.id in state.drafts ? {...state.drafts} : state.drafts
          if (victim.id in drafts) delete drafts[victim.id]
          const queuedByThread = victim.id in state.queuedByThread ? {...state.queuedByThread} : state.queuedByThread
          if (victim.id in queuedByThread) delete queuedByThread[victim.id]
          const modelRefusals = victim.id in state.modelRefusals ? {...state.modelRefusals} : state.modelRefusals
          if (victim.id in modelRefusals) delete modelRefusals[victim.id]
          delete historyStatus[victim.id]
          return {
            threads,
            activeThreadId: id,
            conversations: nextConversations,
            drafts,
            queuedByThread,
            modelRefusals,
            historyStatus,
            lastActiveByFlow: {...state.lastActiveByFlow, [flowId]: id},
          }
        }
      }
      return {
        threads: [...state.threads, thread],
        activeThreadId: id,
        conversations: nextConversations,
        historyStatus,
        lastActiveByFlow: {...state.lastActiveByFlow, [flowId]: id},
      }
    })
    persistFlowThreads(flowId, get().threads)
    return id
  },

  hydrateThreads: flowId => {
    const existing = get().threads.filter(t => t.flowId === flowId)
    if (existing.length > 0) return existing.map(t => t.id)
    const stored = readStoredThreads(flowId)
    if (stored.length === 0) return []
    const newest = stored[stored.length - 1]
    if (!newest) return []
    const active = newest.id
    set(state => ({
      threads: [...state.threads, ...stored],
      activeThreadId: active,
      lastActiveByFlow: {...state.lastActiveByFlow, [flowId]: active},
    }))
    return stored.map(t => t.id)
  },

  switchThread: threadId => {
    const thread = get().threads.find(t => t.id === threadId)
    if (thread) {
      set(state => ({
        activeThreadId: threadId,
        lastActiveByFlow: {...state.lastActiveByFlow, [thread.flowId]: threadId},
      }))
    }
  },

  threadForFlow: flowId => {
    const {threads, lastActiveByFlow} = get()
    const last = lastActiveByFlow[flowId]
    if (last && threads.some(t => t.id === last && t.flowId === flowId)) return last
    for (let i = threads.length - 1; i >= 0; i--) {
      const t = threads[i]
      if (t && t.flowId === flowId) return t.id
    }
    return null
  },

  closeThread: threadId => {
    const slot = get().streams[threadId]
    if (slot?.streamId) {
      chatApi.cancelStream(slot.streamId).catch(() => {})
    }
    const closedFlowId = get().threads.find(t => t.id === threadId)?.flowId
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
      const queuedByThread = threadId in state.queuedByThread ? {...state.queuedByThread} : state.queuedByThread
      if (threadId in queuedByThread) delete queuedByThread[threadId]
      const modelRefusals = threadId in state.modelRefusals ? {...state.modelRefusals} : state.modelRefusals
      if (threadId in modelRefusals) delete modelRefusals[threadId]
      const historyStatus = {...state.historyStatus}
      delete historyStatus[threadId]
      const activeThreadId =
        state.activeThreadId === threadId ? nextThreadOfFlow(remaining, closedFlowId) : state.activeThreadId
      return {
        threads: remaining,
        conversations: next,
        activeThreadId,
        streams,
        drafts,
        queuedByThread,
        modelRefusals,
        historyStatus,
      }
    })
    if (closedFlowId) persistFlowThreads(closedFlowId, get().threads)
  },

  updateThread: (threadId, patch) => {
    set(state => ({
      threads: state.threads.map(t => (t.id === threadId ? {...t, ...patch} : t)),
    }))
    const flowId = get().threads.find(t => t.id === threadId)?.flowId
    if (flowId) persistFlowThreads(flowId, get().threads)
  },

  setThreadModel: (threadId, pin) => {
    const thread = get().threads.find(t => t.id === threadId)
    if (!thread) return
    let next: ThreadPinFields
    if (pin === null) {
      next = {provider: undefined, model: undefined, modelLabel: undefined}
    } else {
      if (!pin.provider || !pin.model) return
      next = {provider: pin.provider, model: pin.model, modelLabel: storableModelLabel(pin.modelLabel, pin.model)}
    }
    if (thread.provider === next.provider && thread.model === next.model && thread.modelLabel === next.modelLabel) {
      return
    }
    get().updateThread(threadId, next)
  },

  getActiveThread: () => {
    const {threads, activeThreadId} = get()
    return threads.find(t => t.id === activeThreadId) ?? null
  },

  getFlowThreads: flowId => {
    return get().threads.filter(t => t.flowId === flowId)
  },

  clearFlowThreads: flowId => {
    set(state => {
      const toRemove = state.threads.filter(t => t.flowId === flowId).map(t => t.id)
      const remaining = state.threads.filter(t => t.flowId !== flowId)
      const next = new Map(state.conversations)
      for (const id of toRemove) next.delete(id)
      const activeThreadId = toRemove.includes(state.activeThreadId ?? '') ? null : state.activeThreadId
      const streams = {...state.streams}
      const drafts = {...state.drafts}
      const queuedByThread = {...state.queuedByThread}
      const modelRefusals = {...state.modelRefusals}
      const historyStatus = {...state.historyStatus}
      for (const id of toRemove) {
        delete streams[id]
        delete drafts[id]
        delete queuedByThread[id]
        delete modelRefusals[id]
        delete historyStatus[id]
      }
      return {
        threads: remaining,
        conversations: next,
        activeThreadId,
        streams,
        drafts,
        queuedByThread,
        modelRefusals,
        historyStatus,
      }
    })
    persistFlowThreads(flowId, get().threads)
  },
})
