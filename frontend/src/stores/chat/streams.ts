import type {StateCreator} from 'zustand'
import type {ChatState} from './state'
import {MAX_CONCURRENT_STREAMS} from './types'
import {persistFlowThreads} from './persistence'

type StreamSlice = Pick<
  ChatState,
  | 'updateStreamingMessage'
  | 'startStream'
  | 'addToolCall'
  | 'replaceStreamTools'
  | 'replaceStreamFixes'
  | 'endStream'
  | 'queueMessage'
  | 'clearQueuedMessage'
  | 'takeQueuedMessage'
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
          fixProposals: [],
        },
      },
    })),

  // addToolCall appends one finished tool execution to the thread's stream
  // slot (the live view of the per-message tool trail).
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
      // A journal replay rebuilds the cards; keep each known card's arrival
      // time so its countdown doesn't restart.
      const seen = new Map(slot.fixProposals.map(c => [c.proposalId, c.receivedAt]))
      const fixProposals = cards.map(c => ({...c, receivedAt: c.receivedAt ?? seen.get(c.proposalId) ?? Date.now()}))
      return {streams: {...state.streams, [threadId]: {...slot, fixProposals}}}
    }),

  // The turn is over: its slot goes, and with it the thread's in-flight
  // record (persisted) — only the record of THIS stream, never a newer one.
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

  queueMessage: (threadId, msg) => set(state => ({queuedByThread: {...state.queuedByThread, [threadId]: msg}})),
  clearQueuedMessage: threadId =>
    set(state => {
      if (!(threadId in state.queuedByThread)) return state
      const next = {...state.queuedByThread}
      delete next[threadId]
      return {queuedByThread: next}
    }),
  takeQueuedMessage: threadId => {
    const msg = get().queuedByThread[threadId]
    if (msg) get().clearQueuedMessage(threadId)
    return msg
  },

  // setFixProposal appends a card unless one with the same proposalId already
  // exists (replay idempotence) — sequential proposals stack, earlier
  // outcomes are kept.
  setFixProposal: (threadId, card) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      if (slot.fixProposals.some(c => c.proposalId === card.proposalId)) return state
      const stamped = {...card, receivedAt: card.receivedAt ?? Date.now()}
      return {streams: {...state.streams, [threadId]: {...slot, fixProposals: [...slot.fixProposals, stamped]}}}
    }),

  // patchFixProposal merges a status/message update into the matching card
  // (a no-op when the card is gone — the stream may have ended) and applies
  // per-item patches (batch decisions) by ruleId.
  patchFixProposal: (threadId, proposalId, patch, itemPatches) =>
    set(state => {
      const slot = state.streams[threadId]
      if (!slot) return state
      const idx = slot.fixProposals.findIndex(c => c.proposalId === proposalId)
      if (idx < 0) return state
      const card = slot.fixProposals[idx]
      if (!card) return state
      let items = card.items
      if (itemPatches && itemPatches.length > 0) {
        items = card.items.map(it => {
          const p = itemPatches.find(ip => ip.ruleId === it.ruleId)
          return p ? {...it, ...p.patch} : it
        })
      }
      const fixProposals = [...slot.fixProposals]
      fixProposals[idx] = {...card, ...patch, items}
      return {streams: {...state.streams, [threadId]: {...slot, fixProposals}}}
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
