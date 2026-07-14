import {describe, it, expect, vi, beforeEach} from 'vitest'
import {renderHook, waitFor, act} from '@testing-library/react'
import {useAIChat} from './useAIChat'
import {useChatStore} from '@/stores/chatStore'
import {useFlowStore} from '@/stores/flowStore'
import {ToastProvider} from '@/components/shared/Toast'
import {chatApi, flowApi} from '@/api'
import type {FlowDocument} from '@/types'

// Integration test for useAIChat's send/stream/cancel/resend orchestration —
// the seam this hook was split across (useChatRequestBuilder +
// useChatStreamEngine). Uses the same SSE-mock pattern as
// hooks/useStreamingMessage.test.tsx so the real streaming machinery runs
// end-to-end instead of being stubbed out.

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
  getEventConnectionState: vi.fn(() => connState),
}))

vi.mock('@/api', () => ({
  chatApi: {
    streamChatMessage: vi.fn(),
    cancelStream: vi.fn().mockResolvedValue(undefined),
    beginStream: vi.fn(),
    resumeStream: vi.fn().mockResolvedValue({text: '', done: false, error: '', tokensOut: 0, tokensIn: 0}),
    getConversation: vi
      .fn()
      .mockResolvedValue({version: 1, flowKey: 'flow1', scope: 'flow', updatedAt: '', messages: []}),
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
function errorEvent(streamId: string, message: string) {
  return {name: 'chat:event', data: {streamId, type: 'error', data: {message}}}
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

function wrapper({children}: {children: React.ReactNode}) {
  return <ToastProvider>{children}</ToastProvider>
}

// renderChat mounts useAIChat against a fresh document and waits for
// useChatConversations' auto-create-thread-on-mount effect to settle.
async function renderChat() {
  useFlowStore.setState({...initialFlowState, document: doc})
  const view = renderHook(() => useAIChat({selectedModel: 'gpt-4'}), {wrapper})
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
  vi.mocked(chatApi.getConversation).mockResolvedValue({
    version: 1,
    flowKey: 'flow1',
    scope: 'flow',
    updatedAt: '',
    messages: [],
  })
  vi.mocked(flowApi.getSourceFiles).mockResolvedValue([])
  vi.mocked(chatApi.resumeStream).mockResolvedValue({text: '', done: false, error: '', tokensOut: 0, tokensIn: 0})
  vi.mocked(chatApi.cancelStream).mockResolvedValue(undefined)
  vi.mocked(chatApi.saveConversation).mockResolvedValue(undefined)
})

describe('useAIChat send/stream/commit', () => {
  it('sends a message, streams chunks, and commits the assistant reply on done', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid-does-not-matter')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hello', [])
    })

    // User message appended immediately, request sent with the right shape.
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(1))
    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
    const req = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0]
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
    // The stream slot is cleared once committed.
    expect(useChatStore.getState().streams[threadId]).toBeUndefined()
    expect(chatApi.saveConversation).toHaveBeenCalledWith(
      'flow1',
      'flow',
      expect.arrayContaining([
        expect.objectContaining({role: 'user', content: 'hello'}),
        expect.objectContaining({role: 'assistant', content: 'Hello!'}),
      ]),
    )
  })

  it('appends an error bubble when the stream reports an error mid-flight', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hi', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())

    act(() => {
      capturedCb!(chunkEvent(sid, 'partial'))
    })
    act(() => {
      capturedCb!(errorEvent(sid, 'provider exploded'))
    })

    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    const messages = useChatStore.getState().getMessages(threadId)
    expect(messages[1].finishReason).toBe('error')
    expect(messages[1].content).toContain('partial')
    expect(messages[1].content).toContain('provider exploded')
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

    // Only the first send reached the API; the second was a no-op per-thread guard.
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
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].clientStreamId!
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
    const sid1 = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(doneEvent(sid1))
    })
    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))

    act(() => {
      result.current.handleResend()
    })

    // Both messages from the first turn are gone, and a new send goes out
    // with the truncated history explicitly included.
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    const secondReq = vi.mocked(chatApi.streamChatMessage).mock.calls[1][0]
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
