import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, act} from '@testing-library/react'
import {useChatStore} from '@/stores/chatStore'
import type {StreamHandler} from '@/hooks/useStreamingMessage'

let capturedHandler: StreamHandler | null = null

vi.mock('@/hooks/useStreamingMessage', () => ({
  useStreamingMessage: (handler: StreamHandler) => {
    capturedHandler = handler
    return {
      registerStream: async () => true,
      cancel: vi.fn(),
      teardownStream: vi.fn(),
      resumeInto: vi.fn(),
    }
  },
}))

vi.mock('@/api', () => ({
  ApiError: class ApiError extends Error {},
  chatApi: {
    streamChatMessage: vi.fn().mockResolvedValue('stream-r4'),
    cancelStream: vi.fn(),
    saveConversation: vi.fn().mockResolvedValue(undefined),
    respondFixDecision: vi.fn().mockResolvedValue(undefined),
  },
  flowApi: {loadFlowFromPath: vi.fn().mockResolvedValue(null)},
  analysisApi: {analyzeFlowById: vi.fn().mockResolvedValue({flowId: 'flow-1', findings: []})},
}))

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({apiUrl: 'http://localhost:9999', token: 't'}),
  }),
}))

import {useChatStreamEngine} from './useChatStreamEngine'
import {chatApi} from '@/api'
import type {ChatMessage} from '@/types'

const META = {provider: 'claude' as never, model: 'claude-sonnet-4'}

// A thread whose server history has loaded: a save on done would happen if
// the engine still asked for one.
function seedThread(messages: ChatMessage[] = []) {
  useChatStore.setState({
    threads: [
      {
        id: 'threadA',
        flowId: 'flow-1',
        title: 't',
        createdAt: '2024',
        contextBlockId: null,
        selectedSourceFiles: [],
        tokensIn: 0,
        tokensOut: 0,
      },
    ],
    conversations: new Map([['threadA', messages]]),
    historyStatus: {threadA: {state: 'loaded' as const, token: 1}},
  })
}

// sendAndFinish sends one turn, lets the create POST resolve, streams one
// chunk and the done event, and returns the committed answer.
async function sendAndFinish(
  result: {current: ReturnType<typeof useChatStreamEngine>},
  opts: Parameters<ReturnType<typeof useChatStreamEngine>['send']>[2],
) {
  let accepted = false
  await act(async () => {
    accepted = result.current.send('threadA', 'hello', opts)
  })
  expect(accepted).toBe(true)
  await act(async () => {
    await Promise.resolve()
  })
  expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
  const sid = useChatStore.getState().streams.threadA.streamId
  await act(async () => {
    capturedHandler!.onChunk('answer', sid)
    capturedHandler!.onDone(3, 2, sid)
  })
  return useChatStore.getState().conversations.get('threadA')!.at(-1)!
}

beforeEach(() => {
  vi.clearAllMocks()
  capturedHandler = null
  vi.stubGlobal('requestAnimationFrame', (cb: () => void) => {
    cb()
    return 0
  })
  vi.stubGlobal('cancelAnimationFrame', () => {})
  useChatStore.setState({threads: [], streams: {}, activeThreadId: null, conversations: new Map()})
})

describe('useChatStreamEngine save on done', () => {
  // The server stores the question at stream start and the finished answer —
  // trail, fix outcomes, finish reason, usage — at stream end; the client's
  // own full-replace save after done was a third write per turn.
  it('does not save a turn the server owns (sent without history)', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedThread()
    const answer = await sendAndFinish(result, META)
    const req = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0]
    expect(req.messages).toBeUndefined()
    // onDone's usage arguments are (tokensOut, tokensIn).
    expect(answer).toMatchObject({role: 'assistant', content: 'answer', finishReason: 'stop', tokensIn: 2, tokensOut: 3})
    expect(useChatStore.getState().streams.threadA).toBeUndefined()
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
  })

  // A resend carries the thread's (locally truncated) history: the server
  // does not persist such a turn, so the client still saves it on done.
  it('still saves a turn that carried its own history', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedThread([{id: 'u0', role: 'user', content: 'earlier', timestamp: 't'}])
    const answer = await sendAndFinish(result, {...META, includeHistory: true})
    const req = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0]
    expect(req.messages).toHaveLength(1)
    expect(answer).toMatchObject({role: 'assistant', content: 'answer', finishReason: 'stop'})
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
    expect(chatApi.saveConversation).toHaveBeenCalledWith(
      'flow-1',
      expect.any(String),
      expect.arrayContaining([expect.objectContaining({content: 'answer'})]),
    )
  })

  // Stop keeps the generated text as an interrupted answer the server never
  // stores: that commit is saved whoever owns the turn.
  it('saves an interrupted server-owned turn', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedThread()
    await act(async () => {
      result.current.send('threadA', 'hello', META)
    })
    await act(async () => {
      await Promise.resolve()
    })
    const sid = useChatStore.getState().streams.threadA.streamId
    await act(async () => {
      capturedHandler!.onChunk('partial', sid)
      result.current.stop('threadA')
    })
    expect(useChatStore.getState().conversations.get('threadA')!.at(-1)).toMatchObject({
      content: 'partial',
      finishReason: 'interrupted',
    })
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
  })
})
