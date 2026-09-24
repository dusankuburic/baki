import {describe, it, expect, vi, beforeEach} from 'vitest'
import {useEffect} from 'react'
import {renderHook, render, waitFor, act} from '@testing-library/react'
import {useAIChat} from './useAIChat'
import {useChatStore} from '@/stores/chatStore'
import {useFlowStore} from '@/stores/flowStore'
import {ToastProvider} from '@/components/shared/Toast'
import ChatStreamProvider from '../ChatStreamProvider'
import {chatApi, flowApi, ApiError} from '@/api'
import type {ChatMessage, ContextPreview, FlowDocument} from '@/types'

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
  // Mirrors the real constructor (message, status, code) so a test can build
  // a coded rejection and exercise the code-first classification.
  ApiError: class ApiError extends Error {
    constructor(
      message: string,
      public status = 0,
      public code: string | null = null,
    ) {
      super(message)
    }
  },
  chatApi: {
    streamChatMessage: vi.fn(),
    cancelStream: vi.fn().mockResolvedValue(undefined),
    beginStream: vi.fn(),
    resumeStream: vi.fn().mockResolvedValue({text: '', done: false, error: '', tokensOut: 0, tokensIn: 0}),
    // The real endpoint returns a bare message array (see api/chat.ts).
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

// The stream engine lives in ChatStreamProvider, above the chat panel.
function wrapper({children}: {children: React.ReactNode}) {
  return (
    <ToastProvider>
      <ChatStreamProvider>{children}</ChatStreamProvider>
    </ToastProvider>
  )
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
  vi.mocked(chatApi.getConversation).mockResolvedValue([])
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
      threadId,
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

// ---------------------------------------------------------------------------
// H1: history restore. The endpoint returns a bare array; reading `.messages`
// off it restored nothing, and the next finished reply then REPLACED the
// server's history with just the new turn.

function msg(id: string, role: ChatMessage['role'], content: string): ChatMessage {
  return {id, role, content, timestamp: '2024-01-01T00:00:00Z'}
}

// seedPersistedThread puts a thread in the store the way a reload does
// (hydrated from localStorage): no messages, no history status yet.
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
  return renderHook(() => useAIChat({selectedModel: 'gpt-4'}), {wrapper})
}

describe('useAIChat history restore (H1)', () => {
  it('shows the stored conversation of a restored thread', async () => {
    seedPersistedThread()
    vi.mocked(chatApi.getConversation).mockResolvedValue([msg('a', 'user', 'q1'), msg('b', 'assistant', 'a1')])
    const {result} = await renderSeeded()

    await waitFor(() => expect(useChatStore.getState().getMessages('t-old')).toHaveLength(2))
    expect(chatApi.getConversation).toHaveBeenCalledWith('flow1', 't-old')
    expect(result.current.historyState).toBe('loaded')
  })

  it('merges history that arrives late BEFORE a message sent meanwhile', async () => {
    seedPersistedThread()
    let resolveLoad: (m: ChatMessage[]) => void = () => {}
    vi.mocked(chatApi.getConversation).mockImplementation(() => new Promise(r => (resolveLoad = r)))
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderSeeded()
    await waitFor(() => expect(result.current.historyState).toBe('loading'))

    await act(async () => {
      result.current.handleSend('new question', [])
    })
    await act(async () => {
      resolveLoad([msg('a', 'user', 'q1'), msg('b', 'assistant', 'a1')])
    })

    await waitFor(() => expect(useChatStore.getState().getMessages('t-old')).toHaveLength(3))
    expect(
      useChatStore
        .getState()
        .getMessages('t-old')
        .map(m => m.content),
    ).toEqual(['q1', 'a1', 'new question'])
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
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(chunkEvent(sid, 'answer'))
    })
    act(() => {
      capturedCb!(doneEvent(sid))
    })

    await waitFor(() => expect(useChatStore.getState().getMessages('t-old')).toHaveLength(2))
    // The server persists the turn itself; a client save would have replaced
    // the (unread) stored history with these two messages.
    expect(chatApi.saveConversation).not.toHaveBeenCalled()
  })

  it('sends the turn message ids so the server stores its copy under them', async () => {
    vi.mocked(chatApi.streamChatMessage).mockResolvedValue('sid')
    const {result} = await renderChat()
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hello', [])
    })
    const req = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0]
    const [userMsg] = useChatStore.getState().getMessages(threadId)
    expect(req.userMessageId).toBe(userMsg.id)
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
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].selectedSourceFiles).toEqual(['Main.txt'])
    await waitFor(() => expect(onConfirmed).toHaveBeenCalledTimes(1))
  })

  it('resets the composer as soon as the message is on screen, not after the network round-trips', async () => {
    vi.mocked(chatApi.previewContext).mockResolvedValue(PREVIEW)
    // The create POST never answers: the reset must not wait for it.
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
    // A new thread takes the panel while the dialog is open.
    act(() => {
      useChatStore.getState().createThread('flow1')
    })
    await waitFor(() => expect(result.current.activeThreadId).not.toBe(original))
    act(() => {
      result.current.confirmContextPreview()
    })

    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1))
    expect(vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].conversationId).toBe(original)
    expect(
      useChatStore
        .getState()
        .getMessages(original)
        .map(m => m.content),
    ).toEqual(['q'])
    // The composer shows the other thread now: its text is left alone.
    expect(onConfirmed).not.toHaveBeenCalled()
  })
})

// ---------------------------------------------------------------------------
// Send races: a send that lost its generation must clean up ITS stream only.

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
    const sid1 = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].clientStreamId!
    act(() => {
      result.current.handleQueue('follow-up', [])
    })

    // Stop frees the thread; the queued follow-up drains into it at once.
    await act(async () => {
      result.current.handleCancelStream()
    })
    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    const sid2 = vi.mocked(chatApi.streamChatMessage).mock.calls[1][0].clientStreamId!
    expect(sid2).not.toBe(sid1)
    expect(useChatStore.getState().streams[threadId]?.streamId).toBe(sid2)

    // The stopped send's POST answers late: only its own stream is torn down.
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
    // Not an orphan question on screen (it would be saved as one), not a
    // title, no stream slot — and the text is back as the thread's draft.
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
    // Another flow's thread is in memory (opened earlier in the session).
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
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(errorEvent(sid, 'daily AI budget exceeded ($1.02 / $1.00, of which $0.00 …)', 'AI_BUDGET_EXCEEDED'))
    })

    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    const bubble = useChatStore.getState().getMessages(threadId)[1]
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
    const {result, rerender} = renderHook(({model}: {model: string}) => useAIChat({selectedModel: model}), {
      wrapper,
      initialProps: {model: 'gpt-4'},
    })
    await waitFor(() => expect(result.current.activeThreadId).not.toBeNull())
    const threadId = result.current.activeThreadId!

    await act(async () => {
      result.current.handleSend('hi', [])
    })
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    rerender({model: 'gpt-5'})
    act(() => {
      capturedCb!(chunkEvent(sid, 'answer'))
    })
    act(() => {
      capturedCb!(doneEvent(sid))
    })

    await waitFor(() => expect(useChatStore.getState().getMessages(threadId)).toHaveLength(2))
    expect(useChatStore.getState().getMessages(threadId)[1].model).toBe('gpt-4')
  })
})

// ---------------------------------------------------------------------------
// H2: the chat panel mounts and unmounts (inspector collapse, mobile drawer,
// pop-out, breakpoint change). Owning the streams there cancelled every reply
// in flight and left its thread stuck on "Stop".

let panelApi: ReturnType<typeof useAIChat> | null = null
function Panel() {
  const api = useAIChat({selectedModel: 'gpt-4'})
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
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      capturedCb!(chunkEvent(sid, 'first half'))
    })

    // The panel goes away mid-reply.
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
    const sid = vi.mocked(chatApi.streamChatMessage).mock.calls[0][0].clientStreamId!
    await waitFor(() => expect(capturedCb).not.toBeNull())
    act(() => {
      panelApi!.handleQueue('follow-up', [])
    })

    view.rerender(<Shell showPanel={false} />)
    act(() => {
      capturedCb!(doneEvent(sid))
    })

    await waitFor(() => expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(2))
    const second = vi.mocked(chatApi.streamChatMessage).mock.calls[1][0]
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

    // The whole shell goes (logout, an error boundary): ending the live slot
    // fires the drain, which must not send into the dead engine.
    view.unmount()
    await act(async () => {})
    expect(chatApi.streamChatMessage).toHaveBeenCalledTimes(1)
  })
})
