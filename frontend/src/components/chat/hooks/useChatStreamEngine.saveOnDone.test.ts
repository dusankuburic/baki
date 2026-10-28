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
      confirmStream: vi.fn(),
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
    getConversation: vi.fn(),
    clearConversation: vi.fn().mockResolvedValue(undefined),
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
import {clearThreadConversation} from '@/lib/conversationSync'
import type {DoneMeta} from '@/lib/chatEvent'
import type {ChatMessage} from '@/types'

const META = {provider: 'claude' as never, model: 'claude-sonnet-4'}

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
  const sid = useChatStore.getState().streams.threadA!.streamId
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
  it('does not save a turn the server owns (sent without history)', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedThread()
    const answer = await sendAndFinish(result, META)
    const req = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0]
    expect(req.messages).toBeUndefined()
    expect(answer).toMatchObject({role: 'assistant', content: 'answer', finishReason: 'stop', tokensIn: 2, tokensOut: 3})
    expect(useChatStore.getState().streams.threadA).toBeUndefined()
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
  })

  it('still saves a turn that carried its own history', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedThread([{id: 'u0', role: 'user', content: 'earlier', timestamp: 't'}])
    const answer = await sendAndFinish(result, {...META, includeHistory: true})
    const req = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0]
    expect(req.messages).toHaveLength(1)
    expect(answer).toMatchObject({role: 'assistant', content: 'answer', finishReason: 'stop'})
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
    expect(chatApi.saveConversation).toHaveBeenCalledWith(
      'flow-1',
      expect.any(String),
      expect.arrayContaining([expect.objectContaining({content: 'answer'})]),
    )
  })

  it('saves an interrupted server-owned turn', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedThread()
    await act(async () => {
      result.current.send('threadA', 'hello', META)
    })
    await act(async () => {
      await Promise.resolve()
    })
    const sid = useChatStore.getState().streams.threadA!.streamId
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

// R-docs-8: the done of a server-owned turn the client cannot adopt (no version on it, or a base other than the one
// it holds — a reload mid-stream reads the version after the question was stored) refreshes the thread's version
// once, with no save, so the next clear or compact is not refused as "changed elsewhere" when no one else wrote.
describe('useChatStreamEngine version refresh after an unadoptable server-owned done', () => {
  function seedVersioned(version: number) {
    seedThread()
    useChatStore.setState({historyStatus: {threadA: {state: 'loaded' as const, token: 1, version}}})
  }

  async function finishWith(result: {current: ReturnType<typeof useChatStreamEngine>}, meta?: DoneMeta) {
    await act(async () => {
      result.current.send('threadA', 'hello', META)
    })
    await act(async () => {
      await Promise.resolve()
    })
    const sid = useChatStore.getState().streams.threadA!.streamId
    await act(async () => {
      capturedHandler!.onChunk('answer', sid)
      capturedHandler!.onDone(3, 2, sid, meta)
    })
    await act(async () => {
      await Promise.resolve()
      await Promise.resolve()
    })
  }

  // The server holds exactly what this client shows.
  const serverHoldsMine = (version: number) =>
    vi.mocked(chatApi.getConversation).mockImplementation(async () => ({
      messages: [...useChatStore.getState().getMessages('threadA')],
      version,
    }))

  it('refreshes once when the base differs from the held version, and a clear then goes through', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedVersioned(5)
    serverHoldsMine(7)
    await finishWith(result, {conversationVersion: 7, conversationBase: 4})

    expect(chatApi.getConversation).toHaveBeenCalledTimes(1)
    expect(useChatStore.getState().getConversationVersion('threadA')).toBe(7)
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
    await expect(clearThreadConversation('threadA', 'flow-1', 'threadA')).resolves.toBe(true)
    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow-1', 'threadA', 7)
  })

  it('refreshes once when the done names no version', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedVersioned(5)
    serverHoldsMine(6)
    await finishWith(result, undefined)
    expect(chatApi.getConversation).toHaveBeenCalledTimes(1)
    expect(useChatStore.getState().getConversationVersion('threadA')).toBe(6)
  })

  it('keeps the held version when another writer really wrote, so a clear still says so', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedVersioned(5)
    vi.mocked(chatApi.getConversation).mockImplementation(async () => ({
      messages: [
        ...useChatStore.getState().getMessages('threadA'),
        {id: 'from-elsewhere', role: 'user', content: 'another tab', timestamp: 't'},
      ],
      version: 8,
    }))
    await finishWith(result, undefined)
    expect(chatApi.getConversation).toHaveBeenCalledTimes(1)
    expect(useChatStore.getState().getConversationVersion('threadA')).toBe(5)
  })

  it('keeps the held version when another writer deleted messages this client still shows', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedThread([
      {id: 'u0', role: 'user', content: 'earlier', timestamp: 't'},
      {id: 'a0', role: 'assistant', content: 'earlier answer', timestamp: 't'},
    ])
    useChatStore.setState({historyStatus: {threadA: {state: 'loaded' as const, token: 1, version: 5}}})
    // Another tab cleared the thread while this turn ran: the server holds only this turn.
    vi.mocked(chatApi.getConversation).mockImplementation(async () => ({
      messages: useChatStore
        .getState()
        .getMessages('threadA')
        .filter(m => m.id !== 'u0' && m.id !== 'a0'),
      version: 9,
    }))
    await finishWith(result, undefined)
    expect(chatApi.getConversation).toHaveBeenCalledTimes(1)
    expect(useChatStore.getState().getConversationVersion('threadA')).toBe(5)
  })

  it('keeps a version that moved while the read was on its way', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedVersioned(5)
    vi.mocked(chatApi.getConversation).mockImplementation(async () => {
      useChatStore.getState().setConversationVersion('threadA', 6)
      return {messages: [...useChatStore.getState().getMessages('threadA')], version: 9}
    })
    await finishWith(result, undefined)
    expect(useChatStore.getState().getConversationVersion('threadA')).toBe(6)
  })

  it('does not refresh a done it adopts, nor one whose answer it saves itself', async () => {
    const {result} = renderHook(() => useChatStreamEngine())
    seedVersioned(5)
    await finishWith(result, {conversationVersion: 6, conversationBase: 5})
    expect(useChatStore.getState().getConversationVersion('threadA')).toBe(6)

    await finishWith(result, {conversationVersion: 9, conversationBase: 2, answerStored: false})
    expect(chatApi.getConversation).not.toHaveBeenCalled()
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(1)
  })
})
