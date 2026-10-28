import type {StateCreator} from 'zustand'
import type {ChatState} from './state'
import {MAX_CONCURRENT_STREAMS, MAX_QUEUED_MESSAGES, type QueuedMessage} from './types'
import {persistFlowThreads} from './persistence'
import {uuid} from '@/lib/uuid'

type QueueState = Pick<ChatState, 'queuedByThread' | 'queuePausedByThread'>

/** The thread's queue replaced; an empty queue drops the thread's key, and its pause with it. */
function withQueue(state: QueueState, threadId: string, queue: QueuedMessage[]): QueueState {
  const queuedByThread = {...state.queuedByThread}
  let queuePausedByThread = state.queuePausedByThread
  if (queue.length > 0) {
    queuedByThread[threadId] = queue
  } else {
    delete queuedByThread[threadId]
    if (threadId in queuePausedByThread) {
      queuePausedByThread = {...queuePausedByThread}
      delete queuePausedByThread[threadId]
    }
  }
  return {queuedByThread, queuePausedByThread}
}

type StreamSlice = Pick<
  ChatState,
  | 'updateStreamingMessage'
  | 'startStream'
  | 'addToolCall'
  | 'replaceStreamTools'
  | 'replaceStreamFixes'
  | 'endStream'
  | 'queueMessage'
  | 'removeQueuedMessage'
  | 'clearQueuedMessage'
  | 'takeQueuedMessage'
  | 'requeueMessage'
  | 'pauseQueue'
  | 'resumeQueue'
  | 'setFixProposal'
  | 'patchFixProposal'
  | 'setStreamMeta'
  | 'updateStream'
  | 'activeStreamCount'
  | 'canStartStream'
>

export const createStreamSlice: StateCreator<ChatState, [], [], StreamSlice> = (set, get) => ({
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
        [threadId]: {
          streamId,
          messageId,
          text: '',
          isThinking: true,
          tokens: 0,
          toolStatus: null,
          toolCalls: [],
          approvals: [],
        },
      },
    })),

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
      const seen = new Map(slot.approvals.map(c => [c.proposalId, c.receivedAt]))
      const approvals = cards.map(c => ({...c, receivedAt: c.receivedAt ?? seen.get(c.proposalId) ?? Date.now()}))
      return {streams: {...state.streams, [threadId]: {...slot, approvals}}}
    }),

  endStream: threadId => {
    const slot = get().streams[threadId]
    if (!slot) return
    const thread = get().threads.find(t => t.id === threadId)
    const endsTurn = !!thread?.inFlight && thread.inFlight.streamId === slot.streamId
    set(state => {
      const next = {...state.streams}
      delete next[threadId]
      if (!endsTurn) return {streams: next}
      return {streams: next, threads: state.threads.map(t => (t.id === threadId ? {...t, inFlight: undefined} : t))}
    })
    if (endsTurn && thread) persistFlowThreads(thread.flowId, get().threads)
  },

  queueMessage: (threadId, msg) => {
    const queue = get().queuedByThread[threadId] ?? []
    if (queue.length >= MAX_QUEUED_MESSAGES) return false
    set(state => ({queuedByThread: {...state.queuedByThread, [threadId]: [...queue, {...msg, id: uuid()}]}}))
    return true
  },
  removeQueuedMessage: (threadId, id) =>
    set(state => {
      const queue = state.queuedByThread[threadId]
      if (!queue?.some(q => q.id === id)) return state
      return withQueue(
        state,
        threadId,
        queue.filter(q => q.id !== id),
      )
    }),
  clearQueuedMessage: threadId =>
    set(state => {
      if (!(threadId in state.queuedByThread)) return state
      return withQueue(state, threadId, [])
    }),
  takeQueuedMessage: threadId => {
    const [head, ...rest] = get().queuedByThread[threadId] ?? []
    if (head) set(state => withQueue(state, threadId, rest))
    return head
  },
  requeueMessage: (threadId, msg) =>
    set(state => withQueue(state, threadId, [msg, ...(state.queuedByThread[threadId] ?? [])])),
  pauseQueue: (threadId, reason) =>
    set(state => {
      // The first reason stands: a Stop that leaves no answer is a Stop, not a failure.
      if (!state.queuedByThread[threadId]?.length || threadId in state.queuePausedByThread) return state
      return {queuePausedByThread: {...state.queuePausedByThread, [threadId]: reason}}
    }),
  resumeQueue: threadId =>
    set(state => {
      if (!(threadId in state.queuePausedByThread)) return state
      const queuePausedByThread = {...state.queuePausedByThread}
      delete queuePausedByThread[threadId]
      return {queuePausedByThread}
    }),

  setFixProposal: (threadId, card) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      if (slot.approvals.some(c => c.proposalId === card.proposalId)) return state
      const stamped = {...card, receivedAt: card.receivedAt ?? Date.now()}
      return {streams: {...state.streams, [threadId]: {...slot, approvals: [...slot.approvals, stamped]}}}
    }),

  patchFixProposal: (threadId, proposalId, patch, itemPatches) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      const idx = slot.approvals.findIndex(c => c.proposalId === proposalId)
      if (idx < 0) return state
      const card = slot.approvals[idx]
      if (!card) return state
      let items = card.items
      if (itemPatches && itemPatches.length > 0) {
        items = card.items.map((it, i) => {
          const p = itemPatches.find(ip => ip.index === i)
          return p ? {...it, ...p.patch} : it
        })
      }
      const approvals = [...slot.approvals]
      approvals[idx] = {...card, ...patch, items}
      return {streams: {...state.streams, [threadId]: {...slot, approvals}}}
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
})
