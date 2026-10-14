import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {useEffect} from 'react'
import {renderHook, render, waitFor, act, screen} from '@testing-library/react'
import {useAIChat} from './useAIChat'
import {useChatStore} from '@/stores/chatStore'
import {useFlowStore} from '@/stores/flowStore'
import {ToastProvider} from '@/components/shared/Toast'
import ChatStreamProvider from '../ChatStreamProvider'
import {chatApi, flowApi, ApiError} from '@/api'
import {threadModelOf} from '@/stores/chatStore'
import {parseConversationPayload} from '@/lib/chatMessage'
import type {TurnModel} from './useChatStreamEngine'
import type {ChatMessage, ContextPreview, FlowDocument} from '@/types'

type EventCb = (ev: {name: string; data: unknown}) => void
type StateCb = (state: string) => void

let connState = 'open'
let capturedCb: EventCb | null = null
const stateListeners = new Set<StateCb>()

vi.mock('@/api/client', () => ({
  subscribeToEvents: vi.fn(async (cb: EventCb) => {
    capturedCb = cb
    return () => {
      capturedCb = null
    }
  }),
  subscribeConnectionState: vi.fn((cb: StateCb) => {
    stateListeners.add(cb)
    cb(connState)
    return () => stateListeners.delete(cb)
  }),
  subscribeEventGap: vi.fn(() => () => {}),
  getEventConnectionState: vi.fn(() => connState),
}))

vi.mock('@/api', () => ({
  ApiError: class ApiError extends Error {
    constructor(
      message: string,
      public status = 0,
      public code: string | null = null,
      public requestId: string | null = null,
      public details?: Record<string, unknown>,
    ) {
      super(message)
    }
  },
  chatApi: {
    streamChatMessage: vi.fn(),
    cancelStream: vi.fn().mockResolvedValue(undefined),
    beginStream: vi.fn(),
    resumeStream: vi.fn().mockResolvedValue({text: '', done: false, error: '', tokensOut: 0, tokensIn: 0}),
    getConversation: vi.fn().mockResolvedValue([]),
    saveConversation: vi.fn().mockResolvedValue(undefined),
    clearConversation: vi.fn().mockResolvedValue(undefined),
    previewContext: vi.fn(),
  },
  flowApi: {
    getSourceFiles: vi.fn().mockResolvedValue([]),
  },
}))

function chunkEvent(streamId: string, content: string) {
  return {name: 'chat:event', data: {streamId, type: 'chunk', data: {content}}}
}
function doneEvent(streamId: string, tokensOut = 3, tokensIn = 2) {
  return {name: 'chat:event', data: {streamId, type: 'done', data: {tokensOut, tokensIn}}}
}
function errorEvent(streamId: string, message: string, code?: string) {
  return {name: 'chat:event', data: {streamId, type: 'error', data: {message, ...(code ? {code} : {})}}}
}

const doc = {
  id: 'flow1',
  name: 'Flow',
  filePath: '/f.txt',
  subflows: [],
  metadata: {blockCount: 0, subflowCount: 0, maxDepth: 0, parsedAt: '', fileSize: 0, rawLineCount: 0},
} as FlowDocument

const initialChatState = useChatStore.getState()
const initialFlowState = useFlowStore.getState()

const TURN: TurnModel = {provider: 'claude', model: 'gpt-4'}

function wrapper({children}: {children: React.ReactNode}) {
  return (
    <ToastProvider>
      <ChatStreamProvider>{children}</ChatStreamProvider>
    </ToastProvider>
  )
}

async function renderChat() {
  useFlowStore.setState({...initialFlowState, document: doc})
  const view = renderHook(() => useAIChat({turnModel: TURN}), {wrapper})
  await waitFor(() => expect(view.result.current.activeThreadId).not.toBeNull())
  return view
}

beforeEach(() => {
  useChatStore.setState(initialChatState, true)
  useFlowStore.setState(initialFlowState, true)
  connState = 'open'
  capturedCb = null
  stateListeners.clear()
  vi.clearAllMocks()
  vi.mocked(chatApi.getConversation).mockResolvedValue({messages: []})
  vi.mocked(flowApi.getSourceFiles).mockResolvedValue([])
  vi.mocked(chatApi.resumeStream).mockResolvedValue({text: '', done: false, error: '', tokensOut: 0, tokensIn: 0})
  vi.mocked(chatApi.cancelStream).mockResolvedValue(undefined)
  vi.mocked(chatApi.saveConversation).mockResolvedValue({})
})

describe('useAIChat send/stream/commit', () => {
  it('sends a message, streams chunks, and commits the assistant reply on done', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid-does-not-matter')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hello', [])
    })

    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(1))
    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
    const req = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0]
    expect(req.flowId).toBe('flow1')
    expect(req.userMessage).toBe('hello')
    expect(req.clientStreamId).toBeTruthy()
    const sid = req.clientStreamId!

    await waitFor(() => expect(capturedCb).not.toBeNull())

    act(() => {
      capturedCb!(chunkEvent(sid, 'Hel'))
    })
    act(() => {
      capturedCb!(chunkEvent(sid, 'lo!'))
    })
    await waitFor(() => expect(useChatStore.getState().streams[threadId]?.text).toBe('Hello!'))

    act(() => {
      capturedCb!(doneEvent(sid, 7, 3))
    })

    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    const messages = useChatStore.getState().getMessages(threadId)
    expect(messages[1]).toMatchObject({
      role: 'assistant',
      content: 'Hello!',
      finishReason: 'stop',
      tokensOut: 7,
      tokensIn: 3,
    })
    expect(useChatStore.getState().streams[threadId]).toBeUndefined()
    expect(req.messages).toBeUndefined()
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
  })

  it('appends an error bubble when the stream reports an error mid-flight', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hi', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())

    act(() => {
      capturedCb!(chunkEvent(sid, 'partial'))
    })
    act(() => {
      capturedCb!(errorEvent(sid, 'provider exploded'))
    })

    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    const messages = useChatStore.getState().getMessages(threadId)
    expect(messages[1]!.finishReason).toBe('error')
    expect(messages[1]!.content).toContain('partial')
    expect(messages[1]!.content).toContain('provider exploded')
  })

  it('rejects a second send on the same thread while one is already streaming', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderChat()

    await act(async () => {
      result.current.handleSend('first', [])
    })
    await waitFor(() => expect(capturedCb).not.toBeNull())
    await act(async () => {
      result.current.handleSend('second', [])
    })

    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
  })
})

describe('useAIChat handleCancelStream', () => {
  it('commits partial text as an interrupted message and cancels the backend stream', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hi', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())

    act(() => {
      capturedCb!(chunkEvent(sid, 'partial answer'))
    })
    await waitFor(() => expect(useChatStore.getState().streams[threadId]?.text).toBe('partial answer'))

    act(() => {
      result.current.handleCancelStream()
    })

    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    const messages = useChatStore.getState().getMessages(threadId)
    expect(messages[1]).toMatchObject({role: 'assistant', content: 'partial answer', finishReason: 'interrupted'})
    expect(useChatStore.getState().streams[threadId]).toBeUndefined()
    expect(chatApi.cancelStream).toHaveBeenCalledWith(sid)
  })

  it('is a no-op when nothing is streaming', async () => {
    const {result} = await renderChat()
    expect(() =>
      act(() => {
        result.current.handleCancelStream()
      }),
    ).not.toThrow()
    expect(chatApi.cancelStream).not.toHaveBeenCalled()
  })
})

describe('useAIChat handleResend', () => {
  it('removes the last user/assistant pair and re-sends with includeHistory', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('first question', [])
    })
    const sid1 = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(doneEvent(sid1))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))

    act(() => {
      result.current.handleResend()
    })

    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    const secondReq = vi.mocked(chatApi.streamChatMessage).mock.calls[1]![0]
    expect(secondReq.userMessage).toBe('first question')
    expect(secondReq.messages).toEqual([])
  })

  it('is a no-op when there is no completed assistant reply yet', async () => {
    const {result} = await renderChat()
    act(() => {
      result.current.handleResend()
    })
    expect(chatApi.streamChatMessage).not.toHaveBeenCalled()
  })
})

function msg(id: string, role: ChatMessage['role'], content: string): ChatMessage {
  return {id, role, content, timestamp: '2024-01-01T00:00:00Z'}
}

function seedPersistedThread(id = 't-old') {
  useChatStore.setState({
    threads: [
      {
        id,
        flowId: 'flow1',
        title: 'Earlier chat',
        createdAt: '2024-01-01T00:00:00Z',
        contextBlockId: null,
        selectedSourceFiles: [],
        tokensIn: 0,
        tokensOut: 0,
      },
    ],
    activeThreadId: id,
  })
}

async function renderSeeded() {
  useFlowStore.setState({...initialFlowState, document: doc})
  return renderHook(() => useAIChat({turnModel: TURN}), {wrapper})
}

describe('useAIChat history restore (H1)', () => {
  it('shows the stored conversation of a restored thread', async () => {
    seedPersistedThread()
    vi.mocked(chatApi.getConversation).mockResolvedValue({
      messages: [msg('a', 'user', 'q1'), msg('b', 'assistant', 'a1')],
    })
    const {result} = await renderSeeded()

    await waitFor(() => expect(useChatStore.getState().getMessages('t-old')).toHaveLength(2))
    expect(chatApi.getConversation).toHaveBeenCalledWith('flow1', 't-old')
    expect(result.current.historyState).toBe('loaded')
  })

  it('merges history that arrives late BEFORE a message sent meanwhile', async () => {
    seedPersistedThread()
    let resolveLoad: (m: {messages: ChatMessage[]}) => void = () => {}
    vi.mocked(chatApi.getConversation).mockImplementation(() => new Promise(r => (resolveLoad = r)))
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderSeeded()
    await waitFor(() => expect(result.current.historyState).toBe('loading'))

    await act(async () => {
      result.current.handleSend('new question', [])
    })
    await act(async () => {
      resolveLoad({messages: [msg('a', 'user', 'q1'), msg('b', 'assistant', 'a1')]})
    })

    await waitFor(() => expect(useChatStore.getState().getMessages('t-old')).toHaveLength(3))
    expect(
      useChatStore
        .getState()
        .getMessages('t-old')
        .map(m => m.content),
    ).toEqual(['q1', 'a1', 'new question'])
  })

  it('re-attaches an in-flight stream reported by the conversation load and commits the resumed done', async () => {
    seedPersistedThread()
    vi.mocked(chatApi.getConversation).mockResolvedValue({
      messages: [msg('a', 'user', 'q1')],
      activeStreamId: 'live-1',
    })
    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'the answer',
      done: true,
      error: '',
      tokensOut: 7,
      tokensIn: 5,
      servedModel: 'gpt-4o',
      inputCostPerM: 2.5,
      outputCostPerM: 10,
    })
    await renderSeeded()

    await waitFor(() => {
      const msgs = useChatStore.getState().getMessages('t-old')
      expect(msgs.some(m => m.role === 'assistant' && m.content === 'the answer')).toBe(true)
    })
    const committed = useChatStore
      .getState()
      .getMessages('t-old')
      .find(m => m.role === 'assistant')!
    expect(committed.servedModel).toBe('gpt-4o')
    expect(committed.inputCostPerM).toBe(2.5)
    expect(committed.tokensIn).toBe(5)
    expect(committed.tokensOut).toBe(7)
    const thread = useChatStore.getState().threads.find(t => t.id === 't-old')!
    expect(thread.tokensIn).toBe(5)
    expect(thread.tokensOut).toBe(7)
    expect(useChatStore.getState().streams['t-old']).toBeUndefined()
  })

  it('drops a reload-reattached stream that is gone and refetches the history', async () => {
    seedPersistedThread()
    let loadCalls = 0
    vi.mocked(chatApi.getConversation).mockImplementation(async () => {
      loadCalls++
      return loadCalls <= 1
        ? {messages: [msg('a', 'user', 'q1')], activeStreamId: 'live-2'}
        : {messages: [msg('a', 'user', 'q1'), msg('b', 'assistant', 'persisted answer')]}
    })
    vi.mocked(chatApi.resumeStream).mockRejectedValue(new ApiError('stream expired', 404, 'STREAM_NOT_FOUND'))
    await renderSeeded()

    await waitFor(() => expect(loadCalls).toBeGreaterThanOrEqual(2))
    await waitFor(() => {
      expect(
        useChatStore
          .getState()
          .getMessages('t-old')
          .map(m => m.content),
      ).toContain('persisted answer')
    })
    expect(useChatStore.getState().streams['t-old']).toBeUndefined()
  })

  it('re-attaches once: the refetch after a lost stream does not re-attach again', async () => {
    seedPersistedThread()
    let loadCalls = 0
    vi.mocked(chatApi.getConversation).mockImplementation(async () => {
      loadCalls++
      return {messages: [msg('a', 'user', 'q1')], activeStreamId: 'live-3'}
    })
    vi.mocked(chatApi.resumeStream).mockRejectedValue(new Error('network down'))
    await renderSeeded()

    await waitFor(() => expect(loadCalls).toBe(2))
    await act(async () => {
      await new Promise(r => setTimeout(r, 50))
    })
    expect(loadCalls).toBe(2)
    expect(chatApi.resumeStream).toHaveBeenCalledTimes(1)
    expect(useChatStore.getState().streams['t-old']).toBeUndefined()
  })

  it('does not re-attach from a stale history load', async () => {
    seedPersistedThread()
    let resolveLoad: (m: {messages: ChatMessage[]; activeStreamId?: string}) => void = () => {}
    vi.mocked(chatApi.getConversation).mockImplementation(() => new Promise(r => (resolveLoad = r)))
    const {result} = await renderSeeded()
    await waitFor(() => expect(result.current.historyState).toBe('loading'))

    act(() => {
      useChatStore.getState().markHistoryLoaded('t-old')
    })
    await act(async () => {
      resolveLoad({messages: [msg('a', 'user', 'q1')], activeStreamId: 'live-4'})
    })

    expect(chatApi.resumeStream).not.toHaveBeenCalled()
    expect(useChatStore.getState().streams['t-old']).toBeUndefined()
    expect(useChatStore.getState().getMessages('t-old')).toHaveLength(0)
  })

  it('never full-replace saves a thread whose history failed to load', async () => {
    seedPersistedThread()
    vi.mocked(chatApi.getConversation).mockRejectedValue(new Error('db down'))
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderSeeded()
    await waitFor(() => expect(result.current.historyState).toBe('error'))

    await act(async () => {
      result.current.handleSend('hello', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(chunkEvent(sid, 'answer'))
    })
    act(() => {
      capturedCb!(doneEvent(sid))
    })

    await waitFor(() => expect(useChatStore.getState().getMessages('t-old')).toHaveLength(2))
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
  })

  it('sends the turn message ids so the server stores its copy under them', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hello', [])
    })
    const req = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0]
    const [userMsg] = useChatStore.getState().getMessages(threadId)
    expect(req.userMessageId).toBe(userMsg!.id)
    expect(req.assistantMessageId).toBe(useChatStore.getState().streams[threadId]?.messageId)
  })
})

describe('useAIChat context preview (consent step)', () => {
  const PREVIEW: ContextPreview = {
    systemPrompt: 's',
    contextText: 'c',
    userMessage: 'q',
    estimatedTokens: 10,
    contextLimit: 1000,
  }

  it('does not send when the preview cannot be loaded', async () => {
    vi.mocked(chatApi.previewContext).mockRejectedValue(new Error('boom'))
    const {result} = await renderChat()

    await act(async () => {
      await result.current.handlePreviewContext('secret question', [], false)
    })

    expect(chatApi.streamChatMessage).not.toHaveBeenCalled()
    expect(result.current.contextPreview).toBeNull()
  })

  it('sends the previewed message with its files on confirm, then clears the composer', async () => {
    vi.mocked(chatApi.previewContext).mockResolvedValue(PREVIEW)
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const onConfirmed = vi.fn()
    const {result} = await renderChat()

    await act(async () => {
      await result.current.handlePreviewContext('q', ['Main.txt'], false, onConfirmed)
    })
    expect(result.current.contextPreview).not.toBeNull()
    expect(onConfirmed).not.toHaveBeenCalled()

    await act(async () => {
      result.current.confirmContextPreview()
    })

    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].selectedSourceFiles).toEqual(['Main.txt'])
    await waitFor(() => expect(onConfirmed).toHaveBeenCalledTimes(1))
  })

  it('resets the composer as soon as the message is on screen, not after the network round-trips', async () => {
    vi.mocked(chatApi.previewContext).mockResolvedValue(PREVIEW)
    vi.mocked(chatApi.streamChatMessage).mockImplementation(() => new Promise(() => {}))
    const onConfirmed = vi.fn()
    const {result} = await renderChat()

    await act(async () => {
      await result.current.handlePreviewContext('q', [], false, onConfirmed)
    })
    act(() => {
      result.current.confirmContextPreview()
    })

    expect(onConfirmed).toHaveBeenCalledTimes(1)
  })

  it('sends a confirmed preview to the thread it was built for, even after a thread switch', async () => {
    vi.mocked(chatApi.previewContext).mockResolvedValue(PREVIEW)
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const onConfirmed = vi.fn()
    const {result} = await renderChat()
    const original = result.current.activeThreadId!

    await act(async () => {
      await result.current.handlePreviewContext('q', [], false, onConfirmed)
    })
    act(() => {
      useChatStore.getState().createThread('flow1')
    })
    await waitFor(() => expect(result.current.activeThreadId).not.toBe(original))
    act(() => {
      result.current.confirmContextPreview()
    })

    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].conversationId).toBe(original)
    expect(
      useChatStore
        .getState()
        .getMessages(original)
        .map(m => m.content),
    ).toEqual(['q'])
    expect(onConfirmed).not.toHaveBeenCalled()
  })
})

describe('useChatStreamEngine send races', () => {
  it('a Stop while the create POST is in flight does not kill the follow-up that took the thread', async () => {
    let resolveFirst: (sid: string) => void = () => {}
    vi.mocked(chatApi.streamChatMessage)
      .mockImplementationOnce(() => new Promise(r => (resolveFirst = r)))
      .mockResolvedValueOnce('sid-2')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('first', [])
    })
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    const sid1 = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    act(() => {
      result.current.handleQueue('follow-up', [])
    })

    await act(async () => {
      result.current.handleCancelStream()
    })
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    const sid2 = vi.mocked(chatApi.streamChatMessage).mock.calls[1]![0].clientStreamId!
    expect(sid2).not.toBe(sid1)
    expect(useChatStore.getState().streams[threadId]?.streamId).toBe(sid2)

    await act(async () => {
      resolveFirst('sid-1')
    })
    expect(useChatStore.getState().streams[threadId]?.streamId).toBe(sid2)
    expect(chatApi.cancelStream).not.toHaveBeenCalledWith(sid2)
  })

  it('a server capacity rejection takes the message back to the composer', async () => {
    vi.mocked(chatApi.streamChatMessage).mockRejectedValue(
      new ApiError('too many chat responses running at once', 429, 'CHAT_CAPACITY_REACHED'),
    )
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hello there', [])
    })

    await waitFor(() => expect(useChatStore.getState().stagedPrompt).toEqual({threadId, text: 'hello there'}))
    expect(useChatStore.getState().getMessages(threadId)).toHaveLength(0)
    expect(useChatStore.getState().threads.find(t => t.id === threadId)?.title).toBe('')
    expect(useChatStore.getState().streams[threadId]).toBeUndefined()
    expect(useChatStore.getState().drafts[threadId]).toBe('hello there')
  })
})

describe("closing the flow's last thread", () => {
  it("replaces it with a fresh thread for the same flow, never another flow's", async () => {
    const {result} = await renderChat()
    const t1 = result.current.activeThreadId!
    act(() => {
      useChatStore.getState().createThread('flow2')
      useChatStore.getState().switchThread(t1)
    })

    act(() => {
      result.current.handleCloseThread(t1)
    })

    await waitFor(() => expect(result.current.activeThread?.flowId).toBe('flow1'))
    expect(result.current.activeThreadId).not.toBe(t1)
    expect(useChatStore.getState().threads.some(t => t.id === t1)).toBe(false)
  })
})

describe('useAIChat error codes', () => {
  it('shows the budget notice for an SSE error coded AI_BUDGET_EXCEEDED', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hi', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(errorEvent(sid, 'daily AI budget exceeded ($1.02 / $1.00, of which $0.00 …)', 'AI_BUDGET_EXCEEDED'))
    })

    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    const bubble = useChatStore.getState().getMessages(threadId)[1]!
    expect(bubble.finishReason).toBe('error')
    expect(bubble.content).toContain('Daily AI budget reached')
    expect(bubble.content).toContain('$1.02 / $1.00')
    expect(bubble.content).not.toContain('Error:')
  })
})

describe('useAIChat model label', () => {
  it('labels the reply with the model it was sent with, not the one selected later', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    useFlowStore.setState({...initialFlowState, document: doc})
    const {result, rerender} = renderHook(
      ({model}: {model: string}) => useAIChat({turnModel: {provider: 'claude', model}}),
      {
        wrapper,
        initialProps: {model: 'gpt-4'},
      },
    )
    await waitFor(() => expect(result.current.activeThreadId).not.toBeNull())
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hi', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    rerender({model: 'gpt-5'})
    act(() => {
      capturedCb!(chunkEvent(sid, 'answer'))
    })
    act(() => {
      capturedCb!(doneEvent(sid))
    })

    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    expect(useChatStore.getState().getMessages(threadId)[1]!.model).toBe('gpt-4')
  })
})

let panelApi: ReturnType<typeof useAIChat> | null = null
function Panel() {
  const api = useAIChat({turnModel: TURN})
  useEffect(() => {
    panelApi = api
  })
  return null
}
function Shell({showPanel}: {showPanel: boolean}) {
  return (
    <ToastProvider>
      <ChatStreamProvider>{showPanel ? <Panel /> : null}</ChatStreamProvider>
    </ToastProvider>
  )
}

describe('replies survive the chat panel unmounting (H2)', () => {
  beforeEach(() => {
    panelApi = null
  })

  it('keeps streaming and commits the reply after the panel unmounts', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    useFlowStore.setState({...initialFlowState, document: doc})
    const view = render(<Shell showPanel />)
    await waitFor(() => expect(panelApi?.activeThreadId).toBeTruthy())
    const threadId = panelApi!.activeThreadId!

    await act(async () => {
      panelApi!.handleSend('long question', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(chunkEvent(sid, 'first half'))
    })

    view.rerender(<Shell showPanel={false} />)
    expect(chatApi.cancelStream).not.toHaveBeenCalled()

    act(() => {
      capturedCb!(chunkEvent(sid, ', second half'))
    })
    act(() => {
      capturedCb!(doneEvent(sid))
    })

    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    expect(useChatStore.getState().getMessages(threadId)[1]).toMatchObject({
      role: 'assistant',
      content: 'first half, second half',
      finishReason: 'stop',
    })
    expect(useChatStore.getState().streams[threadId]).toBeUndefined()
  })

  it('sends a queued follow-up when the reply ends while the panel is hidden', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    useFlowStore.setState({...initialFlowState, document: doc})
    const view = render(<Shell showPanel />)
    await waitFor(() => expect(panelApi?.activeThreadId).toBeTruthy())

    await act(async () => {
      panelApi!.handleSend('first', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      panelApi!.handleQueue('follow-up', [])
    })

    view.rerender(<Shell showPanel={false} />)
    act(() => {
      capturedCb!(doneEvent(sid))
    })

    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    const second = vi.mocked(chatApi.streamChatMessage).mock.calls[1]![0]
    expect(second.userMessage).toBe('follow-up')
    expect(second.model).toBe('gpt-4')
  })

  it('does not drain a queued follow-up into an engine that is being torn down', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    useFlowStore.setState({...initialFlowState, document: doc})
    const view = render(<Shell showPanel />)
    await waitFor(() => expect(panelApi?.activeThreadId).toBeTruthy())

    await act(async () => {
      panelApi!.handleSend('first', [])
    })
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      panelApi!.handleQueue('follow-up', [])
    })

    view.unmount()
    await act(async () => {})
    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
  })
})

const DOCS: TurnModel = {provider: 'azure-foundry', model: 'm/docs000000', modelLabel: 'Docs writer'}
const GPT: TurnModel = {provider: 'azure-foundry', model: 'm/gpt4100000', modelLabel: 'GPT-4.1'}

async function renderWithTurn(turn: TurnModel) {
  useFlowStore.setState({...initialFlowState, document: doc})
  const view = renderHook(({turnModel}: {turnModel: TurnModel}) => useAIChat({turnModel}), {
    wrapper,
    initialProps: {turnModel: turn},
  })
  await waitFor(() => expect(view.result.current.activeThreadId).not.toBeNull())
  return view
}

const pinOf = (threadId: string) => threadModelOf(useChatStore.getState().threads.find(t => t.id === threadId)!)

describe('useAIChat per-thread model memory', () => {
  it('pins the thread to the pair a send went to, and labels the reply with it', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderWithTurn(DOCS)
    const threadId = result.current.activeThreadId!
    expect(pinOf(threadId)).toBeNull()

    await act(async () => {
      result.current.handleSend('document this flow', [])
    })
    const req = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0]
    expect(req).toMatchObject({provider: 'azure-foundry', model: 'm/docs000000'})
    expect(pinOf(threadId)).toEqual({provider: 'azure-foundry', model: 'm/docs000000', modelLabel: 'Docs writer'})

    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(chunkEvent(req.clientStreamId!, 'Here are the docs'))
    })
    act(() => {
      capturedCb!(doneEvent(req.clientStreamId!))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    expect(useChatStore.getState().getMessages(threadId)[1]).toMatchObject({
      provider: 'azure-foundry',
      model: 'm/docs000000',
      modelLabel: 'Docs writer',
    })
  })

  it("a turn on the stand-in for a temporarily unavailable model keeps the thread's own pin", async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderWithTurn({...GPT, pinThread: false})
    const threadId = result.current.activeThreadId!
    act(() => {
      useChatStore.getState().setThreadModel(threadId, DOCS)
    })

    await act(async () => {
      result.current.handleSend('hi', [])
    })
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].model).toBe('m/gpt4100000')
    expect(pinOf(threadId)?.model).toBe('m/docs000000')
  })

  it('MODEL_NOT_AVAILABLE: the text goes back to the composer, the listing refreshes, the notice is raised', async () => {
    vi.mocked(chatApi.streamChatMessage).mockRejectedValue(
      new ApiError('model m/docs000000 is not available', 400, 'MODEL_NOT_AVAILABLE'),
    )
    const {result} = await renderWithTurn(DOCS)
    const threadId = result.current.activeThreadId!
    const epoch0 = useChatStore.getState().providerEpoch

    await act(async () => {
      result.current.handleSend('document this flow', [])
    })

    await waitFor(() => expect(useChatStore.getState().stagedPrompt).toEqual({threadId, text: 'document this flow'}))
    const st = useChatStore.getState()
    expect(st.getMessages(threadId)).toHaveLength(0)
    expect(st.drafts[threadId]).toBe('document this flow')
    expect(st.streams[threadId]).toBeUndefined()
    expect(st.providerEpoch).toBe(epoch0 + 1)
    expect(st.modelRefusals[threadId]).toEqual({...DOCS, epoch: epoch0 + 1})
    expect(pinOf(threadId)?.model).toBe('m/docs000000')
  })

  it('a queued message drains with the pair it was queued with, and a pick made since stays', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const view = await renderWithTurn(DOCS)
    const threadId = view.result.current.activeThreadId!

    await act(async () => {
      view.result.current.handleSend('first', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      view.result.current.handleQueue('follow-up', [])
    })

    act(() => {
      useChatStore.getState().setThreadModel(threadId, GPT)
    })
    view.rerender({turnModel: GPT})
    act(() => {
      capturedCb!(doneEvent(sid))
    })

    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[1]![0]).toMatchObject({
      userMessage: 'follow-up',
      model: 'm/docs000000',
    })
    expect(pinOf(threadId)?.model).toBe('m/gpt4100000')
  })

  it('the context-preview confirm sends with the pair captured when the preview was taken', async () => {
    vi.mocked(chatApi.previewContext).mockResolvedValue({
      systemPrompt: 's',
      contextText: 'c',
      userMessage: 'q',
      estimatedTokens: 10,
      contextLimit: 1000,
    })
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const view = await renderWithTurn(DOCS)

    await act(async () => {
      await view.result.current.handlePreviewContext('q', [], false)
    })
    expect(vi.mocked(chatApi.previewContext).mock.calls[0]![0].model).toBe('m/docs000000')
    view.rerender({turnModel: GPT})
    act(() => {
      view.result.current.confirmContextPreview()
    })

    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].model).toBe('m/docs000000')
  })

  it('a preview refused with MODEL_NOT_AVAILABLE refreshes the listing and raises the notice, sending nothing', async () => {
    vi.mocked(chatApi.previewContext).mockRejectedValue(new ApiError('gone', 400, 'MODEL_NOT_AVAILABLE'))
    const {result} = await renderWithTurn(DOCS)
    const threadId = result.current.activeThreadId!
    const epoch0 = useChatStore.getState().providerEpoch

    await act(async () => {
      await result.current.handlePreviewContext('q', [], false)
    })
    expect(result.current.contextPreview).toBeNull()
    expect(chatApi.streamChatMessage).not.toHaveBeenCalled()
    expect(useChatStore.getState().providerEpoch).toBe(epoch0 + 1)
    expect(useChatStore.getState().modelRefusals[threadId]).toMatchObject({model: 'm/docs000000', epoch: epoch0 + 1})
  })

  it('keeps modelLabel through commit → reload → resend save', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const first = await renderWithTurn(DOCS)
    const threadId = first.result.current.activeThreadId!
    await act(async () => {
      first.result.current.handleSend('document this flow', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(doneEvent(sid))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    const saved = useChatStore.getState().getMessages(threadId)
    expect(saved[1]).toMatchObject({role: 'assistant', modelLabel: 'Docs writer'})
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
    first.unmount()

    const thread = useChatStore.getState().threads.find(t => t.id === threadId)!
    useChatStore.setState(initialChatState, true)
    useChatStore.setState({threads: [thread], activeThreadId: threadId})
    vi.mocked(chatApi.getConversation).mockResolvedValue({
      messages: parseConversationPayload(JSON.parse(JSON.stringify(saved))),
    })
    vi.mocked(chatApi.saveConversation).mockClear()
    vi.mocked(chatApi.streamChatMessage).mockClear()
    capturedCb = null
    const second = renderHook(() => useAIChat({turnModel: DOCS}), {wrapper})
    await waitFor(() => expect(second.result.current.historyState).toBe('loaded'))

    await act(async () => {
      second.result.current.handleSend('and the error handling?', [])
    })
    const sid2 = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(doneEvent(sid2))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(4))
    expect(chatApi.saveConversation).not.toHaveBeenCalled()

    capturedCb = null
    act(() => {
      second.result.current.handleResend()
    })
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    const sid3 = vi.mocked(chatApi.streamChatMessage).mock.calls[1]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(doneEvent(sid3))
    })
    await waitFor(() => expect(chatApi.saveConversation).toHaveBeenCalledTimes(1))
    const resaved = vi.mocked(chatApi.saveConversation).mock.calls[0]![2]
    expect(resaved).toHaveLength(4)
    expect(resaved[1]!).toMatchObject({id: saved[1]!.id, modelLabel: 'Docs writer'})
    expect(resaved[3]).toMatchObject({modelLabel: 'Docs writer'})
  })

  it('never stores an id as the label of the reply or the pin', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderWithTurn({
      provider: 'azure-foundry',
      model: 'm/docs000000',
      modelLabel: 'm/docs000000',
    })
    const threadId = result.current.activeThreadId!
    await act(async () => {
      result.current.handleSend('document this flow', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    expect(pinOf(threadId)).toEqual({provider: 'azure-foundry', model: 'm/docs000000'})
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(doneEvent(sid))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    expect(useChatStore.getState().getMessages(threadId)[1]).not.toHaveProperty('modelLabel')
  })

  it('says at once that a send was refused for its model', async () => {
    vi.mocked(chatApi.streamChatMessage).mockRejectedValue(new ApiError('gone', 400, 'MODEL_NOT_AVAILABLE'))
    const {result} = await renderWithTurn(DOCS)
    await act(async () => {
      result.current.handleSend('document this flow', [])
    })
    expect(await screen.findByText('This model is no longer available')).toBeInTheDocument()
    expect(
      screen.getByText('Your message is back in the composer. Choose another model to send it.'),
    ).toBeInTheDocument()
  })
})

describe('useAIChat in-flight turn record', () => {
  const inFlightOf = (threadId: string) => useChatStore.getState().threads.find(t => t.id === threadId)?.inFlight

  it('records the turn on the thread until it ends — a stand-in’s turn with the stand-in', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderWithTurn({...GPT, pinThread: false})
    const threadId = result.current.activeThreadId!
    act(() => {
      useChatStore.getState().setThreadModel(threadId, DOCS)
    })
    await act(async () => {
      result.current.handleSend('hi', [])
    })
    const req = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0]
    expect(inFlightOf(threadId)).toEqual({
      streamId: req.clientStreamId,
      messageId: req.assistantMessageId,
      provider: 'azure-foundry',
      model: 'm/gpt4100000',
      modelLabel: 'GPT-4.1',
    })
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(doneEvent(req.clientStreamId!))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    expect(inFlightOf(threadId)).toBeUndefined()
    expect(pinOf(threadId)?.model).toBe('m/docs000000')
  })

  it('a reload re-attaches the turn labelled with the model it went to, not the thread’s pin', async () => {
    seedPersistedThread()
    useChatStore.getState().updateThread('t-old', {
      ...DOCS,
      inFlight: {
        streamId: 'live-1',
        messageId: 'a-live-1',
        provider: 'azure-foundry',
        model: GPT.model,
        modelLabel: 'GPT-4.1',
      },
    })
    vi.mocked(chatApi.getConversation).mockResolvedValue({messages: [msg('a', 'user', 'q1')], activeStreamId: 'live-1'})
    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'the answer',
      done: true,
      error: '',
      tokensOut: 2,
      tokensIn: 1,
    })
    await renderSeeded()

    await waitFor(() => expect(useChatStore.getState().getMessages('t-old')).toHaveLength(2))
    expect(useChatStore.getState().getMessages('t-old')[1]).toMatchObject({
      id: 'a-live-1',
      content: 'the answer',
      provider: 'azure-foundry',
      model: 'm/gpt4100000',
      modelLabel: 'GPT-4.1',
    })
    expect(inFlightOf('t-old')).toBeUndefined()
    expect(pinOf('t-old')?.model).toBe('m/docs000000')
  })

  it('drops the record of a turn that ended while the page was away', async () => {
    seedPersistedThread()
    useChatStore.getState().updateThread('t-old', {
      inFlight: {streamId: 'gone-1', provider: 'azure-foundry', model: GPT.model},
    })
    vi.mocked(chatApi.getConversation).mockResolvedValue({
      messages: [msg('a', 'user', 'q1'), msg('b', 'assistant', 'a1')],
    })
    const {result} = await renderSeeded()
    await waitFor(() => expect(result.current.historyState).toBe('loaded'))
    expect(inFlightOf('t-old')).toBeUndefined()
    expect(chatApi.resumeStream).not.toHaveBeenCalled()
  })
})

describe('useAIChat resend refused', () => {
  it('puts the exchange back as it was, and leaves the composer alone', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValueOnce('sid')
    const {result} = await renderWithTurn(DOCS)
    const threadId = result.current.activeThreadId!
    await act(async () => {
      result.current.handleSend('first question', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(doneEvent(sid))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    const before = useChatStore.getState().getMessages(threadId)
    act(() => {
      useChatStore.getState().setDraft(threadId, 'an unrelated draft')
    })
    const saves = vi.mocked(chatApi.saveConversation).mock.calls.length

    vi.mocked(chatApi.streamChatMessage).mockRejectedValueOnce(new ApiError('gone', 400, 'MODEL_NOT_AVAILABLE'))
    act(() => {
      result.current.handleResend()
    })
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(useChatStore.getState().streams[threadId]).toBeUndefined())

    expect(useChatStore.getState().getMessages(threadId)).toEqual(before)
    expect(useChatStore.getState().drafts[threadId]).toBe('an unrelated draft')
    expect(useChatStore.getState().stagedPrompt).toBeNull()
    expect(chatApi.saveConversation).toHaveBeenCalledTimes(saves)
    expect(
      await screen.findByText('Nothing was sent, and the last answer is kept. Choose another model, then try again.'),
    ).toBeInTheDocument()
  })
})

describe('useAIChat while nothing can be sent', () => {
  it('refuses every way of sending, and Regenerate keeps the exchange', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    useFlowStore.setState({...initialFlowState, document: doc})
    const view = renderHook(({blocked}: {blocked: boolean}) => useAIChat({turnModel: TURN, sendBlocked: blocked}), {
      wrapper,
      initialProps: {blocked: false},
    })
    await waitFor(() => expect(view.result.current.activeThreadId).not.toBeNull())
    const threadId = view.result.current.activeThreadId!
    await act(async () => {
      view.result.current.handleSend('q', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0]![0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(doneEvent(sid))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))

    view.rerender({blocked: true})
    await act(async () => {
      view.result.current.handleSend('again', [])
      view.result.current.handleResend()
      view.result.current.handleQueue('later', [])
      await view.result.current.handlePreviewContext('preview', [], false)
    })
    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
    expect(chatApi.previewContext).not.toHaveBeenCalled()
    expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2)
    expect(useChatStore.getState().queuedByThread[threadId]).toBeUndefined()
  })
})

describe('useChatStreamEngine SERVER_RESTARTING', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  const restarting = (sid: string) => errorEvent(sid, 'the server is restarting', 'SERVER_RESTARTING')
  const callAt = (i: number) => vi.mocked(chatApi.streamChatMessage).mock.calls[i]![0]

  async function sendAndListen(text: string) {
    const view = await renderChat()
    const threadId = view.result.current.activeThreadId!
    await act(async () => {
      view.result.current.handleSend(text, [])
    })
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(capturedCb).not.toBeNull())
    return {view, threadId, sid: callAt(0).clientStreamId!}
  }

  async function advance(ms: number) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms)
    })
  }

  it('re-sends the turn once, after a short delay, when its stream ends with SERVER_RESTARTING', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {threadId, sid} = await sendAndListen('first question')
    act(() => {
      capturedCb!(chunkEvent(sid, 'partial'))
    })

    vi.useFakeTimers()
    act(() => {
      capturedCb!(restarting(sid))
    })

    expect(useChatStore.getState().getMessages(threadId)).toHaveLength(1)
    expect(useChatStore.getState().streams[threadId]).toMatchObject({streamId: sid, text: 'partial'})
    expect(useChatStore.getState().streams[threadId]?.toolStatus).toMatch(/server restarted/i)
    await advance(1_900)
    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
    await advance(1_200)
    vi.useRealTimers()
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))

    const retry = callAt(1)
    expect(retry.userMessage).toBe('first question')
    expect(retry.messages).toEqual([])
    expect(retry.clientStreamId).not.toBe(sid)
    expect(
      useChatStore
        .getState()
        .getMessages(threadId)
        .map(m => m.content),
    ).toEqual(['first question'])
    expect(useChatStore.getState().streams[threadId]?.streamId).toBe(retry.clientStreamId)

    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(chunkEvent(retry.clientStreamId!, 'full answer'))
    })
    act(() => {
      capturedCb!(doneEvent(retry.clientStreamId!))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    expect(useChatStore.getState().getMessages(threadId)[1]).toMatchObject({
      content: 'full answer',
      finishReason: 'stop',
    })
    await waitFor(() => expect(chatApi.saveConversation).toHaveBeenCalledTimes(1))
    expect(vi.mocked(chatApi.saveConversation).mock.calls[0]![2].map(m => m.content)).toEqual([
      'first question',
      'full answer',
    ])
  })

  it('shows the error bubble with Retry when the re-sent turn is cut short again (no loop)', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {threadId, sid} = await sendAndListen('q')
    vi.useFakeTimers()
    act(() => {
      capturedCb!(restarting(sid))
    })
    await advance(3_100)
    vi.useRealTimers()
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    const sid2 = callAt(1).clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())

    vi.useFakeTimers()
    act(() => {
      capturedCb!(chunkEvent(sid2, 'second partial'))
    })
    act(() => {
      capturedCb!(restarting(sid2))
    })
    await advance(30_000)

    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2)
    const msgs = useChatStore.getState().getMessages(threadId)
    expect(msgs).toHaveLength(2)
    expect(msgs[1]).toMatchObject({role: 'assistant', finishReason: 'error'})
    expect(msgs[1]!.content).toContain('second partial')
    expect(msgs[1]!.content).toMatch(/server restarted/i)
    expect(useChatStore.getState().streams[threadId]).toBeUndefined()
  })

  it('retries a create refused with 503 SERVER_RESTARTING once, after its Retry-After', async () => {
    vi.mocked(chatApi.streamChatMessage)
      .mockRejectedValueOnce(
        new ApiError('Service Unavailable', 503, 'SERVER_RESTARTING', null, {retryAfterSeconds: 5}),
      )
      .mockResolvedValueOnce('sid')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    vi.useFakeTimers()
    await act(async () => {
      result.current.handleSend('hello', [])
    })
    await advance(0)
    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
    expect(useChatStore.getState().getMessages(threadId)).toHaveLength(1)
    expect(useChatStore.getState().streams[threadId]?.toolStatus).toMatch(/server restarted/i)

    await advance(4_900)
    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
    await advance(1_200)
    vi.useRealTimers()
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    const sid = callAt(1).clientStreamId!
    expect(sid).toBeTruthy()
    expect(callAt(1).userMessage).toBe('hello')
    expect(useChatStore.getState().streams[threadId]).toMatchObject({streamId: sid, toolStatus: null})

    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(chunkEvent(sid, 'answer'))
    })
    act(() => {
      capturedCb!(doneEvent(sid))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    expect(
      useChatStore
        .getState()
        .getMessages(threadId)
        .map(m => m.content),
    ).toEqual(['hello', 'answer'])
  })

  it('shows the error bubble when the retried create is refused again', async () => {
    vi.mocked(chatApi.streamChatMessage).mockRejectedValue(
      new ApiError('Service Unavailable', 503, 'SERVER_RESTARTING'),
    )
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    vi.useFakeTimers()
    await act(async () => {
      result.current.handleSend('hello', [])
    })
    await advance(30_000)

    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2)
    const msgs = useChatStore.getState().getMessages(threadId)
    expect(msgs.map(m => m.role)).toEqual(['user', 'assistant'])
    expect(msgs[1]).toMatchObject({finishReason: 'error'})
    expect(msgs[1]!.content).toMatch(/server restarted/i)
    expect(useChatStore.getState().streams[threadId]).toBeUndefined()
  })

  it('spends one retry per turn: a turn whose create was retried is not re-sent when its stream is cut short', async () => {
    vi.mocked(chatApi.streamChatMessage)
      .mockRejectedValueOnce(new ApiError('Service Unavailable', 503, 'SERVER_RESTARTING'))
      .mockResolvedValueOnce('sid')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    vi.useFakeTimers()
    await act(async () => {
      result.current.handleSend('hello', [])
    })
    await advance(3_100)
    vi.useRealTimers()
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(capturedCb).not.toBeNull())

    vi.useFakeTimers()
    act(() => {
      capturedCb!(restarting(callAt(1).clientStreamId!))
    })
    await advance(30_000)

    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2)
    expect(useChatStore.getState().getMessages(threadId)[1]).toMatchObject({finishReason: 'error'})
  })

  it('a Stop during the wait keeps the partial answer and sends nothing', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {view, threadId, sid} = await sendAndListen('q')
    act(() => {
      capturedCb!(chunkEvent(sid, 'partial'))
    })

    vi.useFakeTimers()
    act(() => {
      capturedCb!(restarting(sid))
    })
    act(() => {
      view.result.current.handleCancelStream()
    })
    await advance(30_000)

    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
    expect(useChatStore.getState().getMessages(threadId)[1]).toMatchObject({
      content: 'partial',
      finishReason: 'interrupted',
    })
    expect(useChatStore.getState().streams[threadId]).toBeUndefined()
  })

  it('leaves a turn that proposed a fix to the user: the error bubble, no automatic resend', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {threadId, sid} = await sendAndListen('fix it')
    act(() => {
      capturedCb!({
        name: 'chat:event',
        data: {streamId: sid, type: 'fix_proposal', data: {proposalId: 'p1', ruleId: 'r1', summary: 'Rename'}},
      })
    })

    vi.useFakeTimers()
    act(() => {
      capturedCb!(restarting(sid))
    })
    await advance(30_000)

    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
    expect(useChatStore.getState().getMessages(threadId)[1]).toMatchObject({finishReason: 'error'})
  })
})

describe('useAIChat adopts the version a finished turn names (ADR 0031, addendum)', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  const withVersion = (streamId: string, type: 'done' | 'error', base: number, version: number, extra = {}) => ({
    name: 'chat:event',
    data: {
      streamId,
      type,
      data: {tokensOut: 3, tokensIn: 2, conversationBase: base, conversationVersion: version, ...extra},
    },
  })
  const callAt = (i: number) => vi.mocked(chatApi.streamChatMessage).mock.calls[i]![0]

  async function renderLoaded(version = 0) {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const view = await renderChat()
    const threadId = view.result.current.activeThreadId!
    // A new thread is born loaded at version 0 and reads nothing; a version is what a read of a stored one would give.
    await waitFor(() => expect(useChatStore.getState().historyStatus[threadId]?.state).toBe('loaded'))
    if (version) useChatStore.getState().setConversationVersion(threadId, version)
    return {view, threadId}
  }

  async function send(view: Awaited<ReturnType<typeof renderLoaded>>['view'], text: string, nth: number) {
    await act(async () => {
      view.result.current.handleSend(text, [])
    })
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(nth))
    await waitFor(() => expect(capturedCb).not.toBeNull())
    return callAt(nth - 1).clientStreamId!
  }

  it('two streamed turns in a row neither save nor reload: each done moves the held version', async () => {
    const {view, threadId} = await renderLoaded(0)
    const loadsAfterOpen = vi.mocked(chatApi.getConversation).mock.calls.length // a new thread reads nothing

    const sid1 = await send(view, 'first', 1)
    act(() => {
      capturedCb!(chunkEvent(sid1, 'one'))
    })
    act(() => {
      capturedCb!(withVersion(sid1, 'done', 0, 2))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    expect(useChatStore.getState().getConversationVersion(threadId)).toBe(2)

    const sid2 = await send(view, 'second', 2)
    // The request of the second turn carries no history: the server has it.
    expect(callAt(1).messages).toBeUndefined()
    act(() => {
      capturedCb!(chunkEvent(sid2, 'two'))
    })
    act(() => {
      capturedCb!(withVersion(sid2, 'done', 2, 4))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(4))

    expect(useChatStore.getState().getConversationVersion(threadId)).toBe(4)
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
    expect(chatApi.getConversation).toHaveBeenCalledTimes(loadsAfterOpen)

    // A clear after those turns is made on the version the server is at.
    await act(async () => {
      view.result.current.handleClearThread()
    })
    await waitFor(() => expect(chatApi.clearConversation).toHaveBeenCalled())
    expect(chatApi.clearConversation).toHaveBeenCalledWith('flow1', expect.any(String), 4)
  })

  it('does not adopt a version when the thread had moved on since the turn began', async () => {
    const {view, threadId} = await renderLoaded(5)
    const sid = await send(view, 'q', 1)
    act(() => {
      capturedCb!(withVersion(sid, 'done', 3, 9))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))

    expect(useChatStore.getState().getConversationVersion(threadId)).toBe(5)
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
  })

  it('a done that names the stored version needs no client save even when the turn was not marked server-owned', async () => {
    // A stream re-attached after a reload loses the send-time ownership flag; the version proves the server stored it.
    const {view, threadId} = await renderLoaded(0)
    const sid = await send(view, 'q', 1)
    useChatStore
      .getState()
      .updateThread(threadId, {inFlight: {streamId: sid, messageId: 'm', provider: 'claude', model: 'gpt-4'}})
    act(() => {
      capturedCb!(chunkEvent(sid, 'answer'))
    })
    act(() => {
      capturedCb!(withVersion(sid, 'done', 0, 2))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
  })

  it("a restart error names the question's version; the re-sent turn reuses the question's id and saves on it without a conflict", async () => {
    const {view, threadId} = await renderLoaded(0)
    const sid = await send(view, 'first question', 1)
    const firstId = callAt(0).userMessageId
    act(() => {
      capturedCb!(chunkEvent(sid, 'partial'))
    })

    vi.useFakeTimers()
    act(() => {
      capturedCb!(withVersion(sid, 'error', 0, 1, {message: 'the server is restarting', code: 'SERVER_RESTARTING'}))
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_100)
    })
    vi.useRealTimers()
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))

    // The question the server stored as `firstId` is the one asked again: nothing to resurrect after the answer.
    expect(callAt(1).userMessageId).toBe(firstId)
    expect(
      useChatStore
        .getState()
        .getMessages(threadId)
        .map(m => m.id),
    ).toEqual([firstId])
    expect(useChatStore.getState().getConversationVersion(threadId)).toBe(1)

    const sid2 = callAt(1).clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(chunkEvent(sid2, 'full answer'))
    })
    act(() => {
      capturedCb!(doneEvent(sid2))
    })
    await waitFor(() => expect(chatApi.saveConversation).toHaveBeenCalledTimes(1))
    const [, , saved, base] = vi.mocked(chatApi.saveConversation).mock.calls[0]!
    expect(saved.map(m => m.content)).toEqual(['first question', 'full answer'])
    expect(saved[0]!.id).toBe(firstId)
    expect(base).toBe(1)
    expect(chatApi.getConversation).not.toHaveBeenCalled()
  })
})
