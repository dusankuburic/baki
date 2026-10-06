// Facade: chatStore was decomposed into src/stores/chat/ — types, the
// localStorage persistence layer, and one store composed from state slices
// (history, messages, streams, threads, settings; see chat/store.ts). This
// module keeps the original import path working for every consumer.
export {
  MAX_CONCURRENT_STREAMS,
  MAX_THREADS,
  conversationKey,
  isProviderId,
  threadPinKey,
  threadModelOf,
} from './chat/types'
export {LEGACY_FLOW_SCOPE} from './chat/persistence'
export {useChatStore} from './chat/store'
export type {
  ChatThread,
  InFlightTurn,
  ThreadModel,
  ModelRefusal,
  FixProposalItem,
  FixProposalCard,
  StreamSlot,
  QueuedMessage,
  HistoryState,
} from './chat/types'
