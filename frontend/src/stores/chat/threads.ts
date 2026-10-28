import type {StateCreator} from 'zustand'
import {chatApi} from '@/api'
import {uuid} from '@/lib/uuid'
import {storableModelLabel} from '@/lib/modelIds'
import {anchorOf, hasBlock, resolveAnchor} from '@/lib/blockAnchor'
import {useFlowStore} from '../flowStore'
import type {ChatState} from './state'
import {MAX_THREADS} from './types'
import {conversationKey} from './types'
import type {ChatThread, HistoryStatus, ThreadPinFields} from './types'
import {logger} from '@/lib/logger'
import {mayHoldStoredConversation, nextHistoryToken} from './history'
import {persistFlowThreads, readStoredThreads} from './persistence'
import {serverThreadIndexAvailable} from './serverIndex'

function nextThreadOfFlow(threads: readonly ChatThread[], flowId: string | undefined): string | null {
  if (!flowId) return null
  for (let i = threads.length - 1; i >= 0; i--) {
    const t = threads[i]
    if (t && t.flowId === flowId) return t.id
  }
  return null
}

/**
 * Deletes an evicted thread's stored conversation where the server keeps no index of the user's threads (the
 * desktop app, a web deployment without accounts; R-frontend-c26), once its flow's stored index no longer lists it
 * (so it cannot come back on the next hydrate), on the version this client holds. A conflict means another tab or
 * session is writing it, so it stays; any failure is logged. Where the server keeps an index, an evicted thread
 * stays reachable through it — the next sync of the flow's list brings it back — so eviction only drops it from
 * this client and deletes nothing.
 */
function clearEvicted(victim: ChatThread, stored: boolean, version: number | undefined): void {
  if (serverThreadIndexAvailable()) return
  // Only on a version this client holds. A thread whose history it never loaded (hydrated, never opened) has none,
  // and an unversioned clear could wipe a conversation another tab is using: its row is left on the server.
  if (!stored || version === undefined) return
  // Loaded on use: a static import would put the settings store in the API's module graph (api → flowStore →
  // chatStore → here), so a test that mocks '@/api' around its original would bind the settings store to the real API.
  import('@/lib/chatHistory')
    .then(({conversationHistoryEnabled}) => {
      if (!conversationHistoryEnabled()) return
      return chatApi.clearConversation(victim.flowId, conversationKey(victim), version)
    })
    .catch(err => logger.warn('Could not delete the evicted thread’s conversation', err))
}

type ThreadSlice = Pick<
  ChatState,
  | 'createThread'
  | 'hydrateThreads'
  | 'switchThread'
  | 'threadForFlow'
  | 'closeThread'
  | 'updateThread'
  | 'setContextBlock'
  | 'remapContextBlocks'
  | 'setThreadModel'
  | 'getActiveThread'
  | 'getFlowThreads'
  | 'clearFlowThreads'
>

export const createThreadSlice: StateCreator<ChatState, [], [], ThreadSlice> = (set, get) => ({
  createThread: flowId => {
    const id = uuid()
    // The thread the cap evicts, and what of it the server holds: read before the eviction drops its state.
    const evicted: {current: {thread: ChatThread; stored: boolean; version?: number} | null} = {current: null}
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
        // A new thread has nothing stored: version 0 is what its first save is compared against.
        [id]: {state: 'loaded', token: nextHistoryToken(), version: 0},
      }
      let threads = [...state.threads, thread]
      if (threads.length > MAX_THREADS) {
        // The oldest idle thread of the creating flow first: another flow's thread goes only when none of this
        // flow's can.
        const idle = (t: ChatThread) => t.id !== id && t.id !== state.activeThreadId && !(t.id in state.streams)
        const victim = threads.find(t => t.flowId === flowId && idle(t)) ?? threads.find(idle)
        if (victim) {
          evicted.current = {
            thread: victim,
            stored: mayHoldStoredConversation(state, victim.id),
            version: state.historyStatus[victim.id]?.version,
          }
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
    if (evicted.current) {
      const {thread: victim, stored, version} = evicted.current
      if (victim.flowId !== flowId) persistFlowThreads(victim.flowId, get().threads)
      clearEvicted(victim, stored, version)
    }
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
    // Block ids are minted per parse, so a restored thread's id is stale whenever the flow was parsed again since
    // it was persisted (every desktop restart): it follows its anchor into the document already on screen.
    const doc = useFlowStore.getState().document
    if (doc?.id === flowId) get().remapContextBlocks(null, doc)
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

  // State only: the stream engine owns cancellation (R-frontend-17) — the close path stops the stream first
  // (engine.abandon), and the engine cancels any stream whose slot vanished without it.
  closeThread: threadId => {
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

  setContextBlock: (threadId, blockId, doc) => {
    get().updateThread(threadId, {
      contextBlockId: blockId,
      contextBlockAnchor: blockId ? anchorOf(doc, blockId) : null,
      contextBlockLost: undefined,
    })
  },

  // A re-parse (an applied fix, a save, a restore, a reopened file) re-mints every block id: each thread grounded
  // on a block of the flow follows it by its content anchor, or is cleared and remembers which block went. The
  // previous parse, when there is one, names the block for a thread persisted before anchors existed; a raw id
  // never reaches the notice.
  remapContextBlocks: (prev, next) => {
    let changed = false
    const threads = get().threads.map(t => {
      if (t.flowId !== next.id || !t.contextBlockId || hasBlock(next, t.contextBlockId)) return t
      changed = true
      const anchor = t.contextBlockAnchor ?? anchorOf(prev, t.contextBlockId)
      const id = anchor ? resolveAnchor(next, anchor) : null
      if (id) return {...t, contextBlockId: id, contextBlockAnchor: anchorOf(next, id)}
      return {...t, contextBlockId: null, contextBlockAnchor: null, contextBlockLost: anchor?.name ?? ''}
    })
    if (!changed) return
    set({threads})
    persistFlowThreads(next.id, threads)
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
