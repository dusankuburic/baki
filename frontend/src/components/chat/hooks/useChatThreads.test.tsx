import {describe, it, expect, beforeEach, vi} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useChatThreads, COMPACT_KEEP_PAIRS, lastExchanges} from './useChatThreads'
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
  it('clear deletes the backend conversation, not just the local one', async () => {
    const id = seedThread(2)
    const {result} = renderThreads(id)

    await act(async () => result.current.handleClearContext())

    expect(useChatStore.getState().getMessages(id)).toHaveLength(0)
    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', id)
  })

  it('clear targets the thread key even for a block-scoped thread', async () => {
    const id = seedThread(2, 'block-7')
    const {result} = renderThreads(id)

    await act(async () => result.current.handleClearContext())

    // The conversation key is the thread id, not the (about to be reset)
    // block scope — so resetting the scope cannot clear the wrong conversation.
    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', id)
    expect(useChatStore.getState().threads.find(t => t.id === id)?.contextBlockId).toBeNull()
  })

  it('compact persists the trimmed history so the next prompt is actually cheaper', async () => {
    const id = seedThread(6)
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    const kept = useChatStore.getState().getMessages(id)
    expect(kept).toHaveLength(COMPACT_KEEP_PAIRS * 2)
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
    const [flowId, scope, saved] = vi.mocked(chatApi.saveConversation).mock.calls[0]
    expect(flowId).toBe('flow1')
    expect(scope).toBe(id)
    expect(saved.map(m => m.id)).toEqual(kept.map(m => m.id))
  })

  it('compact is a no-op below the keep threshold and writes nothing', async () => {
    const id = seedThread(1)
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    expect(useChatStore.getState().getMessages(id)).toHaveLength(2)
    // Still writes the (unchanged) list rather than diverging from the server.
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
  })
})

// The server copy changes FIRST; the panel follows only if that worked. A
// failed request used to leave an emptied/compacted panel over an untouched
// stored conversation the model kept answering from.
describe('useChatThreads failure paths', () => {
  it('a failed clear keeps the thread as it was', async () => {
    const id = seedThread(2)
    vi.mocked(chatApi.clearConversation).mockRejectedValueOnce(new Error('403'))
    const {result} = renderThreads(id)

    await act(async () => result.current.handleClearContext())

    expect(useChatStore.getState().getMessages(id)).toHaveLength(4)
  })

  it('a failed compact keeps every message locally', async () => {
    const id = seedThread(6)
    vi.mocked(chatApi.saveConversation).mockRejectedValueOnce(new Error('403'))
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    expect(useChatStore.getState().getMessages(id)).toHaveLength(12)
  })

  it('refuses to compact a thread whose server history has not loaded', async () => {
    const id = seedThread(6)
    useChatStore.setState(s => ({historyStatus: {...s.historyStatus, [id]: {state: 'error', token: 99}}}))
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    // The kept slice would be cut from an incomplete view and REPLACE the
    // stored conversation.
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
    expect(useChatStore.getState().getMessages(id)).toHaveLength(12)
  })

  it('never saves transient error bubbles as part of the compacted history', async () => {
    const id = seedThread(3)
    useChatStore
      .getState()
      .appendMessage(id, {...msg('boom', 'assistant'), finishReason: 'error', content: '*Error: provider exploded*'})
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    const saved = vi.mocked(chatApi.saveConversation).mock.calls[0][2]
    expect(saved.some(m => m.finishReason === 'error')).toBe(false)
  })
})

// Screen and server must hold the SAME compacted history, cut on a user turn:
// trimming the two by different rules left the stored copy opening with an
// answer to a question that was gone whenever an error bubble sat near the end.
describe('compact keeps whole exchanges', () => {
  it('applies the exact list it persisted, error bubbles dropped', async () => {
    const id = seedThread(4)
    useChatStore.getState().appendMessage(id, {...msg('boom', 'assistant'), finishReason: 'error'})
    const {result} = renderThreads(id)

    await act(async () => result.current.handleCompact())

    const saved = vi.mocked(chatApi.saveConversation).mock.calls[0][2]
    expect(saved.map(m => m.id)).toEqual(['u1', 'a1', 'u2', 'a2', 'u3', 'a3'])
    expect(
      useChatStore
        .getState()
        .getMessages(id)
        .map(m => m.id),
    ).toEqual(saved.map(m => m.id))
  })

  it('lastExchanges keeps the last N user turns with their replies', () => {
    const list = [
      msg('u0', 'user'),
      msg('a0', 'assistant'),
      msg('u1', 'user'),
      msg('a1', 'assistant'),
      msg('u2', 'user'),
    ]
    expect(lastExchanges(list, 2).map(m => m.id)).toEqual(['u1', 'a1', 'u2'])
    expect(lastExchanges(list, 9)).toHaveLength(5)
  })
})
