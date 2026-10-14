import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {renderHook, waitFor} from '@testing-library/react'
import {useChatStore, conversationKey, LEGACY_FLOW_SCOPE} from '@/stores/chatStore'
import {chatApi} from '@/api'
import {useChatConversations} from './useChatConversations'
import type {ChatMessage, FlowDocument} from '@/types'

vi.mock('@/platform/guards', () => ({isTauri: () => true}))

vi.mock('@/api', () => ({
  chatApi: {
    getConversation: vi.fn(),
    saveConversation: vi.fn(),
    clearConversation: vi.fn(),
  },
  flowApi: {getSourceFiles: vi.fn().mockResolvedValue([])},
}))

const doc = {id: 'flow1', name: 'Flow', filePath: '/f.txt', subflows: []} as unknown as FlowDocument
const initial = useChatStore.getState()
const legacyMessage: ChatMessage = {
  id: 'old1',
  role: 'user',
  content: 'from before threads',
  timestamp: '2024-01-01T00:00:00Z',
}

beforeEach(() => {
  useChatStore.setState(initial, true)
  vi.clearAllMocks()
  vi.mocked(chatApi.saveConversation).mockResolvedValue({version: 4})
  vi.mocked(chatApi.clearConversation).mockResolvedValue(undefined)
})

afterEach(() => {
  localStorage.clear()
})

describe('importing the legacy flow conversation into a new thread', () => {
  it('keeps the thread version through the load, saves on it and adopts the version the save answers', async () => {
    vi.mocked(chatApi.getConversation).mockResolvedValue({messages: [legacyMessage]})
    renderHook(() => useChatConversations({doc, flowThreads: [], activeThreadId: null}))

    await waitFor(() => expect(chatApi.saveConversation).toHaveBeenCalledTimes(1))
    const thread = useChatStore.getState().threads.find(t => t.flowId === 'flow1')!
    expect(chatApi.getConversation).toHaveBeenCalledWith('flow1', LEGACY_FLOW_SCOPE)
    // A new thread starts at version 0: the save is made on it, not blind.
    expect(chatApi.saveConversation).toHaveBeenCalledWith('flow1', conversationKey(thread), [legacyMessage], 0)
    await waitFor(() => expect(useChatStore.getState().getConversationVersion(thread.id)).toBe(4))
    await waitFor(() => expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', LEGACY_FLOW_SCOPE))
  })

  it('keeps the thread version when the legacy conversation cannot be read', async () => {
    vi.mocked(chatApi.getConversation).mockRejectedValue(new Error('down'))
    renderHook(() => useChatConversations({doc, flowThreads: [], activeThreadId: null}))

    await waitFor(() => {
      const thread = useChatStore.getState().threads.find(t => t.flowId === 'flow1')
      expect(thread && useChatStore.getState().historyStatus[thread.id]?.state).toBe('loaded')
    })
    const thread = useChatStore.getState().threads.find(t => t.flowId === 'flow1')!
    expect(useChatStore.getState().getConversationVersion(thread.id)).toBe(0)
  })
})
