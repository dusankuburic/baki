import {create} from 'zustand'
import {registerStoreReset} from '../storeRegistry'
import type {ChatState} from './state'
import {createHistorySlice} from './history'
import {createMessagesSlice} from './messages'
import {createStreamSlice} from './streams'
import {createThreadSlice} from './threads'
import {createSettingsSlice} from './settings'
import {purgeLegacyThreadKeys} from './persistence'

export const useChatStore = create<ChatState>()((...a) => ({
  threads: [],
  activeThreadId: null,
  conversations: new Map(),
  streams: {},
  queuedByThread: {},
  queuePausedByThread: {},
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

registerStoreReset(() => {
  useChatStore.setState({
    threads: [],
    activeThreadId: null,
    conversations: new Map(),
    streams: {},
    stagedPrompt: null,
    providerEpoch: 0,
    modelPickerRequest: 0,
    modelRefusals: {},
    drafts: {},
    queuedByThread: {},
    queuePausedByThread: {},
    historyStatus: {},
    lastActiveByFlow: {},
  })
  purgeLegacyThreadKeys()
})
