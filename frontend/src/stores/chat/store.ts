import {create} from 'zustand'
import {registerStoreReset} from '../storeRegistry'
import type {ChatState} from './state'
import {createHistorySlice} from './history'
import {createMessagesSlice} from './messages'
import {createStreamSlice} from './streams'
import {createThreadSlice} from './threads'
import {createSettingsSlice} from './settings'
import {purgeLegacyThreadKeys} from './persistence'

// The single chat store instance: the state slices composed into one
// create() call (zustand stores are singletons — see ./state.ts for the
// state shape and the per-slice modules for the actions).
export const useChatStore = create<ChatState>()((...a) => ({
  threads: [],
  activeThreadId: null,
  conversations: new Map(),
  streams: {},
  queuedByThread: {},
  selectedProvider: 'claude',
  providerEpoch: 0,
  modelPickerRequest: 0,
  modelRefusals: {},
  stagedPrompt: null,
  drafts: {},
  historyStatus: {},
  lastActiveByFlow: {},

  ...createHistorySlice(...a),
  ...createMessagesSlice(...a),
  ...createStreamSlice(...a),
  ...createThreadSlice(...a),
  ...createSettingsSlice(...a),
}))

// Reset on logout (see storeRegistry).
registerStoreReset(() => {
  useChatStore.setState({
    threads: [],
    activeThreadId: null,
    conversations: new Map(),
    streams: {},
    stagedPrompt: null,
    selectedProvider: 'claude',
    providerEpoch: 0,
    modelPickerRequest: 0,
    modelRefusals: {},
    drafts: {},
    // Queued (unsent) follow-ups are session state — they must not survive
    // logout (F1.7).
    queuedByThread: {},
    historyStatus: {},
    lastActiveByFlow: {},
  })
  // Unscoped legacy thread lists must not outlive the session on a shared
  // browser (per-user v2 lists stay — they only load for their owner).
  purgeLegacyThreadKeys()
})
