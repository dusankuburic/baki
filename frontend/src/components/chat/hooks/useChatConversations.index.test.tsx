import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {act, renderHook, waitFor} from '@testing-library/react'
import {useChatStore} from '@/stores/chatStore'
import {chatApi} from '@/api'
import {useChatConversations} from './useChatConversations'
import {registerCurrentUserResolver} from '@/stores/storeRegistry'
import type {FlowDocument} from '@/types'

vi.mock('@/platform/guards', () => ({isTauri: () => false}))

vi.mock('@/api', () => ({
  chatApi: {
    getConversation: vi.fn(),
    saveConversation: vi.fn(),
    clearConversation: vi.fn(),
    listThreads: vi.fn(),
    setThreadTitle: vi.fn(),
  },
  flowApi: {getSourceFiles: vi.fn().mockResolvedValue([])},
}))

const doc = {id: 'flow1', name: 'Flow', filePath: '/f.txt', subflows: []} as unknown as FlowDocument
const initial = useChatStore.getState()

beforeEach(() => {
  // The server keeps an index for whoever is signed in.
  registerCurrentUserResolver(() => 'user-1')
  useChatStore.setState(initial, true)
  vi.clearAllMocks()
  vi.mocked(chatApi.getConversation).mockResolvedValue({messages: [], version: 0})
  vi.mocked(chatApi.setThreadTitle).mockResolvedValue({})
})

afterEach(() => {
  registerCurrentUserResolver(() => null)
  vi.useRealTimers()
  localStorage.clear()
})

// R-frontend-c26 (c): opening a flow's chat rebuilds its thread list from the server's index, and names go there.
describe('the server thread index in the chat', () => {
  it("lists the user's threads of the flow when its chat opens, and shows them", async () => {
    vi.mocked(chatApi.listThreads).mockResolvedValue({
      items: [{id: 's1', title: 'Retry loop', createdAt: 'c', updatedAt: 'u', messageCount: 2, version: 3}],
    })
    renderHook(() => useChatConversations({doc, flowThreads: [], activeThreadId: null}))

    await waitFor(() => expect(chatApi.listThreads).toHaveBeenCalledWith('flow1', {limit: 50}))
    await waitFor(() =>
      expect(
        useChatStore
          .getState()
          .getFlowThreads('flow1')
          .find(t => t.id === 's1')?.title,
      ).toBe('Retry loop'),
    )
  })

  it("sends a thread's new name to the index", async () => {
    vi.mocked(chatApi.listThreads).mockResolvedValue({items: []})
    renderHook(() => useChatConversations({doc, flowThreads: [], activeThreadId: null}))
    await waitFor(() => expect(useChatStore.getState().getFlowThreads('flow1')).toHaveLength(1))
    const id = useChatStore.getState().getFlowThreads('flow1')[0]!.id

    vi.useFakeTimers()
    act(() => {
      useChatStore.getState().updateThread(id, {title: 'Named'})
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000)
    })
    expect(chatApi.setThreadTitle).toHaveBeenCalledWith('flow1', id, 'Named')
  })
})
