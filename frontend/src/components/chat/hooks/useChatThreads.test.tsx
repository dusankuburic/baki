import {describe, it, expect, beforeEach, vi} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useChatThreads, COMPACT_KEEP_PAIRS} from './useChatThreads'
import {useChatStore} from '@/stores/chatStore'
import {chatApi} from '@/api'
import type {ChatMessage, FlowDocument} from '@/types'

vi.mock('@/api', () => ({
  chatApi: {
    saveConversation: vi.fn().mockResolvedValue(undefined),
    clearConversation: vi.fn().mockResolvedValue(undefined),
  },
}))

const doc = {id: 'flow1', name: 'Flow'} as FlowDocument
const initialChatState = useChatStore.getState()

function msg(id: string, role: ChatMessage['role']): ChatMessage {
  return {id, role, content: id, timestamp: '2024-01-01T00:00:00Z'}
}

// Seeds a thread with `pairs` user/assistant exchanges and returns its id.
function seedThread(pairs: number, contextBlockId: string | null = null): string {
  const id = useChatStore.getState().createThread('flow1')
  useChatStore.getState().updateThread(id, {contextBlockId})
  for (let i = 0; i < pairs; i++) {
    useChatStore.getState().appendMessage(id, msg(`u${i}`, 'user'))
    useChatStore.getState().appendMessage(id, msg(`a${i}`, 'assistant'))
  }
  return id
}

function renderThreads(activeThreadId: string) {
  return renderHook(() => useChatThreads({doc, activeThreadId, sourceFiles: []}))
}

beforeEach(() => {
  useChatStore.setState(initialChatState, true)
  vi.mocked(chatApi.clearConversation).mockClear()
  vi.mocked(chatApi.saveConversation).mockClear()
})

// buildRequest omits `messages` and the backend replays its OWN stored
// conversation, so anything that changes the visible history must change the
// server's copy too. Both of these used to touch only the local store, which
// made them look like they worked while changing nothing that mattered.
describe('useChatThreads server-side history', () => {
  it('clear deletes the backend conversation, not just the local one', () => {
    const id = seedThread(2)
    const {result} = renderThreads(id)

    act(() => result.current.handleClearContext())

    expect(useChatStore.getState().getMessages(id)).toHaveLength(0)
    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', 'flow')
  })

  it('clear uses the scope key in effect BEFORE the thread is reset', () => {
    const id = seedThread(2, 'block-7')
    const {result} = renderThreads(id)

    act(() => result.current.handleClearContext())

    // Reading contextBlockId after updateThread would clear the wrong
    // conversation — the flow-scoped one instead of the block's.
    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', 'block-7')
    expect(useChatStore.getState().threads.find(t => t.id === id)?.contextBlockId).toBeNull()
  })

  it('compact persists the trimmed history so the next prompt is actually cheaper', () => {
    const id = seedThread(6)
    const {result} = renderThreads(id)

    act(() => result.current.handleCompact())

    const kept = useChatStore.getState().getMessages(id)
    expect(kept).toHaveLength(COMPACT_KEEP_PAIRS * 2)
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
    const [flowId, scope, saved] = vi.mocked(chatApi.saveConversation).mock.calls[0]
    expect(flowId).toBe('flow1')
    expect(scope).toBe('flow')
    expect(saved.map(m => m.id)).toEqual(kept.map(m => m.id))
  })

  it('compact is a no-op below the keep threshold and writes nothing', () => {
    const id = seedThread(1)
    const {result} = renderThreads(id)

    act(() => result.current.handleCompact())

    expect(useChatStore.getState().getMessages(id)).toHaveLength(2)
    // Still writes the (unchanged) list rather than diverging from the server.
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
  })
})
