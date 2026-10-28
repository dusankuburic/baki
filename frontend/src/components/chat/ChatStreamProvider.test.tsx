import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, act} from '@testing-library/react'
import {useChatStore} from '@/stores/chatStore'
import {ToastProvider} from '@/components/shared/Toast'
import type {ChatMessage} from '@/types'

// The drain against a stand-in engine: send records the message and starts a stream with a new message id, or
// refuses when `refuse` is set.
const sent: string[] = []
let refuse = false
let nextAnswer = 0
const fakeEngine = {
  send: vi.fn((threadId: string, text: string) => {
    if (refuse) return false
    sent.push(text)
    const st = useChatStore.getState()
    st.appendMessage(threadId, {id: `u-${text}`, role: 'user', content: text, timestamp: 't'})
    st.startStream(threadId, `s-${text}`, `a${++nextAnswer}`)
    return true
  }),
}
vi.mock('./hooks/useChatStreamEngine', () => ({useChatStreamEngine: () => fakeEngine}))

import ChatStreamProvider from './ChatStreamProvider'

const THREAD = 't1'

function seed() {
  useChatStore.setState({
    activeThreadId: THREAD,
    threads: [
      {
        id: THREAD,
        flowId: 'f1',
        title: 'T',
        createdAt: 't',
        contextBlockId: null,
        selectedSourceFiles: [],
        tokensIn: 0,
        tokensOut: 0,
      },
    ],
    conversations: new Map([[THREAD, [{id: 'u0', role: 'user', content: 'first', timestamp: 't'}]]]),
    streams: {},
    queuedByThread: {},
    queuePausedByThread: {},
  })
  nextAnswer = 0
  useChatStore.getState().startStream(THREAD, 's0', 'a0')
}

/** Ends the running stream the way the engine does: the answer (if any) lands, then the slot ends. */
async function endTurn(finishReason?: ChatMessage['finishReason']) {
  await act(async () => {
    const st = useChatStore.getState()
    const slot = st.streams[THREAD]!
    if (finishReason) {
      st.appendMessage(THREAD, {id: slot.messageId, role: 'assistant', content: 'reply', timestamp: 't', finishReason})
    }
    st.endStream(THREAD)
    await Promise.resolve()
  })
  await act(async () => {
    await Promise.resolve()
  })
}

const queued = () => useChatStore.getState().queuedByThread[THREAD]?.map(q => q.text)
const paused = () => useChatStore.getState().queuePausedByThread[THREAD]

function mount() {
  render(
    <ToastProvider>
      <ChatStreamProvider>
        <div />
      </ChatStreamProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  sent.length = 0
  refuse = false
  fakeEngine.send.mockClear()
  seed()
})

describe('ChatStreamProvider queue drain', () => {
  function queueTwo() {
    useChatStore.getState().queueMessage(THREAD, {text: 'q1', files: []})
    useChatStore.getState().queueMessage(THREAD, {text: 'q2', files: []})
  }

  it('sends the next follow-up after an answer', async () => {
    mount()
    queueTwo()
    await endTurn('stop')
    expect(sent).toEqual(['q1'])
    expect(queued()).toEqual(['q2'])
  })

  it('holds the queue after a failed turn, keeps every item, and sends again once resumed', async () => {
    mount()
    queueTwo()
    await endTurn('error')
    expect(sent).toEqual([])
    expect(queued()).toEqual(['q1', 'q2'])
    expect(paused()).toBe('error')

    await act(async () => {
      useChatStore.getState().resumeQueue(THREAD)
      await Promise.resolve()
    })
    expect(sent).toEqual(['q1'])
    expect(queued()).toEqual(['q2'])
  })

  it('holds the queue after a stopped turn', async () => {
    mount()
    queueTwo()
    await endTurn('interrupted')
    expect(sent).toEqual([])
    expect(paused()).toBe('stopped')
  })

  it('holds the queue when the turn ended without an answer (a refused or withdrawn send)', async () => {
    mount()
    queueTwo()
    await endTurn()
    expect(sent).toEqual([])
    expect(paused()).toBe('error')
  })

  it('puts a follow-up the engine refuses back at the head, holds the queue and says so', async () => {
    mount()
    queueTwo()
    refuse = true
    await endTurn('stop')
    expect(fakeEngine.send).toHaveBeenCalledTimes(1)
    expect(queued()).toEqual(['q1', 'q2'])
    expect(paused()).toBe('refused')
    expect(await screen.findByText('Your queued message was not sent')).toBeInTheDocument()
  })
})
