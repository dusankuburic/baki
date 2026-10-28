import {describe, it, expect, expectTypeOf, vi, beforeEach, afterEach} from 'vitest'
import {renderHook} from '@testing-library/react'
import {useStreamingMessage, type StreamHandler} from './useStreamingMessage'
import {chatApi} from '@/api'
import type {ErrorUsage} from '@/lib/chatEvent'
import {notifyBackendRestarted} from '@/lib/backendRestart'

type EventCb = (ev: {name: string; data: unknown}) => void
type StateCb = (state: string) => void
type GapCb = () => void

let connState = 'open'
let capturedCb: EventCb | null = null
const stateListeners = new Set<StateCb>()
const gapListeners = new Set<GapCb>()

function setConnState(s: string) {
  connState = s
  stateListeners.forEach(l => l(s))
}

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
  subscribeEventGap: vi.fn((cb: GapCb) => {
    gapListeners.add(cb)
    return () => gapListeners.delete(cb)
  }),
  getEventConnectionState: vi.fn(() => connState),
}))

vi.mock('@/api', () => ({
  chatApi: {
    resumeStream: vi.fn(),
    cancelStream: vi.fn().mockResolvedValue(undefined),
  },
}))

function makeHandler() {
  return {
    onChunk: vi.fn(),
    onReplace: vi.fn(),
    onDone: vi.fn(),
    onError: vi.fn(),
    onToolStatus: vi.fn(),
    onToolResult: vi.fn(),
    onFixProposal: vi.fn(),
    onFixDecision: vi.fn(),
    onResumeState: vi.fn(),
    onAppend: vi.fn(),
    getAccLength: vi.fn(() => 0),
  }
}

function chunkEvent(streamId: string, content: string) {
  return {name: 'chat:event', data: {streamId, type: 'chunk', data: {content}}}
}

beforeEach(() => {
  vi.useFakeTimers()
  connState = 'open'
  capturedCb = null
  stateListeners.clear()
  gapListeners.clear()
  vi.clearAllMocks()
  vi.mocked(chatApi.resumeStream).mockResolvedValue({text: '', done: false, error: '', tokensOut: 0, tokensIn: 0})
  vi.mocked(chatApi.cancelStream).mockResolvedValue(undefined)
})

afterEach(() => {
  vi.useRealTimers()
})

describe('registerStream open-wait timeout', () => {
  it('rejects after the timeout when the connection never opens', async () => {
    connState = 'connecting'
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))

    const p = result.current.registerStream('s1')
    const rejected = expect(p).rejects.toThrow(/Could not connect to the event stream/)
    await vi.advanceTimersByTimeAsync(15_000)
    await rejected

    expect(capturedCb).toBeNull()
    expect(handler.onError).not.toHaveBeenCalled()
  })

  it('resolves as soon as the connection opens', async () => {
    connState = 'connecting'
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))

    const p = result.current.registerStream('s1')
    await vi.advanceTimersByTimeAsync(1_000)
    setConnState('open')
    await expect(p).resolves.toBe(true)
    expect(capturedCb).not.toBeNull()
  })
})

describe('stall probe', () => {
  it('finishes a stream whose done event was dropped, via resume', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    capturedCb!(chunkEvent('s1', 'hel'))
    capturedCb!(chunkEvent('s1', 'lo'))
    expect(handler.onChunk).toHaveBeenCalledTimes(2)

    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'hello world',
      done: true,
      error: '',
      tokensOut: 5,
      tokensIn: 2,
    })
    await vi.advanceTimersByTimeAsync(30_000)

    expect(chatApi.resumeStream).toHaveBeenCalledWith('s1', 0)
    expect(handler.onReplace).toHaveBeenCalledWith('hello world', 's1')
    expect(handler.onDone).toHaveBeenCalledWith(5, 2, 's1', undefined)
  })

  it('surfaces a dropped error event via resume', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: '',
      done: false,
      error: 'provider exploded',
      errorCode: 'PROVIDER_UNAVAILABLE',
      tokensOut: 0,
      tokensIn: 0,
    })
    await vi.advanceTimersByTimeAsync(30_000)

    expect(handler.onError).toHaveBeenCalledWith('provider exploded', 's1', 'PROVIDER_UNAVAILABLE', undefined)
  })

  it("passes a live error event's code to the handler", async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    capturedCb!({
      name: 'chat:event',
      data: {
        streamId: 's1',
        type: 'error',
        data: {message: 'daily AI budget exceeded ($1.02 / $1.00)', code: 'AI_BUDGET_EXCEEDED'},
      },
    })

    expect(handler.onError).toHaveBeenCalledWith(
      'daily AI budget exceeded ($1.02 / $1.00)',
      's1',
      'AI_BUDGET_EXCEEDED',
      undefined,
    )
  })

  it('keeps a stream alive through heartbeats during a long silent phase', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    for (let i = 0; i < 15; i++) {
      await vi.advanceTimersByTimeAsync(20_000)
      capturedCb!({name: 'chat:event', data: {streamId: 's1', type: 'heartbeat', data: {}}})
    }

    expect(chatApi.resumeStream).not.toHaveBeenCalled()
    expect(handler.onError).not.toHaveBeenCalled()
    expect(handler.onChunk).not.toHaveBeenCalled()
  })

  it('gives up after repeated no-progress probes', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    vi.mocked(chatApi.resumeStream).mockResolvedValue({text: '', done: false, error: '', tokensOut: 0, tokensIn: 0})

    await vi.advanceTimersByTimeAsync(30_000)
    await vi.advanceTimersByTimeAsync(30_000)
    expect(handler.onError).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(30_000)

    expect(chatApi.resumeStream).toHaveBeenCalledTimes(3)
    expect(handler.onError).toHaveBeenCalledWith(expect.stringContaining('stalled'), 's1')
  })

  it('treats buffer growth as progress and keeps waiting', async () => {
    const handler = makeHandler()
    handler.getAccLength.mockReturnValue(0)
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'partial',
      done: false,
      error: '',
      tokensOut: 0,
      tokensIn: 0,
    })
    await vi.advanceTimersByTimeAsync(30_000)

    expect(handler.onReplace).toHaveBeenCalledWith('partial', 's1')
    expect(handler.onError).not.toHaveBeenCalled()
    expect(handler.onDone).not.toHaveBeenCalled()
  })

  it('does not probe while chunks keep flowing', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    for (let i = 0; i < 5; i++) {
      await vi.advanceTimersByTimeAsync(20_000)
      capturedCb!(chunkEvent('s1', 'x'))
    }

    expect(chatApi.resumeStream).not.toHaveBeenCalled()
  })

  it('a probe the server answered with an error status still counts as a miss', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    vi.mocked(chatApi.resumeStream).mockRejectedValue(Object.assign(new Error('not found'), {status: 404}))
    await vi.advanceTimersByTimeAsync(3 * 30_000)

    expect(handler.onError).toHaveBeenCalledWith(expect.stringContaining('stalled'), 's1')
  })

  it('a probe that got no answer is not a miss: it says nothing about the stream, and probing goes on', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    vi.mocked(chatApi.resumeStream).mockRejectedValue(new Error('network'))
    await vi.advanceTimersByTimeAsync(5 * 30_000)

    expect(handler.onError).not.toHaveBeenCalled()
    expect(chatApi.resumeStream).toHaveBeenCalledTimes(5)
  })
})

describe('stall probe while the event stream is down (R-streaming-6)', () => {
  // The client's buffer as the engine keeps it: a probe's onReplace sets it.
  function trackingHandler() {
    const handler = makeHandler()
    let acc = 0
    handler.onChunk.mockImplementation((t: string) => (acc += t.length))
    handler.onReplace.mockImplementation((t: string) => (acc = t.length))
    handler.getAccLength.mockImplementation(() => acc)
    return handler
  }

  it('offline for minutes mid-answer (every probe fails) does not end the stream as stalled', async () => {
    const handler = trackingHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'partial'))
    setConnState('reconnecting')

    vi.mocked(chatApi.resumeStream).mockRejectedValue(new TypeError('Failed to fetch'))
    await vi.advanceTimersByTimeAsync(150_000)

    expect(handler.onError).not.toHaveBeenCalled()
  })

  it('a reconnecting stream the server still runs is not counted as stalled, growing or not', async () => {
    const handler = trackingHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'par'))
    setConnState('reconnecting')

    const live = (text: string) => ({text, done: false, error: '', tokensOut: 0, tokensIn: 0})
    vi.mocked(chatApi.resumeStream)
      .mockResolvedValueOnce(live('partial'))
      .mockResolvedValueOnce(live('partial and more'))
      .mockResolvedValue(live('partial and more'))
    await vi.advanceTimersByTimeAsync(6 * 30_000)

    expect(chatApi.resumeStream).toHaveBeenCalledTimes(6)
    expect(handler.onError).not.toHaveBeenCalled()
    expect(handler.onReplace).toHaveBeenLastCalledWith('partial and more', 's1')
  })

  it('commits a stream whose /resume answers done within one probe interval, though SSE is reconnecting', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'par'))
    setConnState('reconnecting')

    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'partial and the rest',
      done: true,
      error: '',
      tokensOut: 7,
      tokensIn: 3,
    })
    await vi.advanceTimersByTimeAsync(30_000)

    expect(chatApi.resumeStream).toHaveBeenCalledWith('s1', 0)
    expect(handler.onDone).toHaveBeenCalledWith(7, 3, 's1', undefined)
  })
})

describe('a backend restart (R-frontend-c7)', () => {
  it('ends every stream in flight as interrupted, says the backend restarted, and never resumes it', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    // Its create was answered: the old process ran it.
    result.current.confirmStream('s1')
    await result.current.registerStream('s2')
    // The mock keeps the last subscriber: s2's.
    const s2Events = capturedCb!
    s2Events(chunkEvent('s2', 'partial'))

    notifyBackendRestarted()

    expect(handler.onError).toHaveBeenCalledTimes(2)
    expect(handler.onError).toHaveBeenCalledWith(expect.stringMatching(/backend restarted/i), 's1', 'BACKEND_RESTARTED')
    expect(handler.onError).toHaveBeenCalledWith(expect.stringMatching(/backend restarted/i), 's2', 'BACKEND_RESTARTED')

    setConnState('reconnecting')
    setConnState('open')
    await vi.advanceTimersByTimeAsync(60_000)
    expect(chatApi.resumeStream).not.toHaveBeenCalled()
    expect(chatApi.cancelStream).not.toHaveBeenCalled()
    // Unsubscribed: the new process's events never reach it.
    expect(capturedCb).toBeNull()
  })

  it('leaves a stream the old process never ran: it goes on against the new backend, nothing cancelled', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('live')
    result.current.confirmStream('live')
    // A send still waiting for the events stream: its create has not been sent yet.
    setConnState('reconnecting')
    const waiting = result.current.registerStream('starting')
    await vi.advanceTimersByTimeAsync(0)
    const startingEvents = capturedCb!

    notifyBackendRestarted()
    setConnState('open')

    await expect(waiting).resolves.toBe(true)
    expect(handler.onError).toHaveBeenCalledTimes(1)
    expect(handler.onError).toHaveBeenCalledWith(expect.any(String), 'live', 'BACKEND_RESTARTED')
    startingEvents(chunkEvent('starting', 'from the new process'))
    expect(handler.onChunk).toHaveBeenCalledWith('from the new process', 'starting')
    expect(chatApi.cancelStream).not.toHaveBeenCalled()
  })
})

describe('reconnect resume of a lost stream', () => {
  function goneError(status: number) {
    return Object.assign(new Error('forbidden'), {status})
  }
  async function reconnect() {
    setConnState('reconnecting')
    setConnState('open')
    await vi.advanceTimersByTimeAsync(0)
  }

  it.each([403, 404, 410])('reports a live stream as interrupted at once when the resume answers %i', async status => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'partial'))

    vi.mocked(chatApi.resumeStream).mockRejectedValue(goneError(status))
    await reconnect()

    expect(chatApi.resumeStream).toHaveBeenCalledWith('s1', 0)
    expect(handler.onError).toHaveBeenCalledTimes(1)
    expect(handler.onError).toHaveBeenCalledWith(expect.stringContaining('connection'), 's1', 'STREAM_INTERRUPTED')
  })

  it('counts a confirmed stream as live before its first event', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    result.current.confirmStream('s1')

    vi.mocked(chatApi.resumeStream).mockRejectedValue(goneError(403))
    await reconnect()

    expect(handler.onError).toHaveBeenCalledWith(expect.any(String), 's1', 'STREAM_INTERRUPTED')
  })

  it('does not report a stream whose create has not answered yet', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    vi.mocked(chatApi.resumeStream).mockRejectedValue(goneError(404))
    await reconnect()

    expect(handler.onError).not.toHaveBeenCalled()
  })

  it('keeps waiting when the resume fails transiently', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'partial'))

    vi.mocked(chatApi.resumeStream).mockRejectedValue(goneError(503))
    await reconnect()
    vi.mocked(chatApi.resumeStream).mockRejectedValue(new Error('network'))
    await reconnect()

    expect(handler.onError).not.toHaveBeenCalled()
  })

  it('passes a SERVER_RESTARTING the resume reports to the handler with its code', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'partial'))

    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: '',
      done: false,
      error: 'the server is restarting',
      errorCode: 'SERVER_RESTARTING',
      tokensOut: 0,
      tokensIn: 0,
    })
    await reconnect()

    expect(handler.onError).toHaveBeenCalledWith('the server is restarting', 's1', 'SERVER_RESTARTING', undefined)
  })
})

describe('done meta (served model)', () => {
  function doneEvent(streamId: string, data: Record<string, unknown>) {
    return {name: 'chat:event', data: {streamId, type: 'done', data}}
  }
  // Rates an older server still sends: the client drops them.
  const RATES = {inputCostPerM: 2.5, outputCostPerM: 10}

  it('passes the served model of a live done to the handler and drops any rates', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    capturedCb!(chunkEvent('s1', 'hi'))
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 1, servedModel: 'gpt-4o', ...RATES}))

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {servedModel: 'gpt-4o'})
  })

  it('passes the stored conversation version a done names, and the version the turn began from', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    capturedCb!(chunkEvent('s1', 'hi'))
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 1, conversationVersion: 12, conversationBase: 10}))

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {conversationVersion: 12, conversationBase: 10})
  })

  it('passes a done that says the server could not store the answer (R-tool-loop-20)', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    capturedCb!(chunkEvent('s1', 'hi'))
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 1, answerStored: false}))

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {answerStored: false})
  })

  it('passes the unstored answer a resume reports after the done', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'hi'))
    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'hi there',
      done: true,
      error: '',
      tokensOut: 3,
      tokensIn: 1,
      answerStored: false,
    })

    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 2}))
    await vi.advanceTimersByTimeAsync(0)

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {answerStored: false})
  })

  it('keeps the conversation version through a chunk-mismatch resume that does not know it', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'hi'))
    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'hi there',
      done: true,
      error: '',
      tokensOut: 3,
      tokensIn: 1,
    })

    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 2, conversationVersion: 12, conversationBase: 10}))
    await vi.advanceTimersByTimeAsync(0)

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {conversationVersion: 12, conversationBase: 10})
  })

  it('passes no meta for a plain done', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    capturedCb!(chunkEvent('s1', 'hi'))
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 1}))

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', undefined)
  })

  it('takes the served model from a chunk-mismatch resume and drops any rates', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'hi'))
    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'hi there',
      done: true,
      error: '',
      tokensOut: 3,
      tokensIn: 1,
      servedModel: 'gpt-4o',
    })

    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 2, ...RATES}))
    await vi.advanceTimersByTimeAsync(0)

    expect(chatApi.resumeStream).toHaveBeenCalledWith('s1', 0)
    expect(handler.onReplace).toHaveBeenCalledWith('hi there', 's1')
    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {servedModel: 'gpt-4o'})
  })

  it('forwards the served model a stall-probe resume reports', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'hel'))
    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'hello',
      done: true,
      error: '',
      tokensOut: 5,
      tokensIn: 2,
      servedModel: 'gpt-4o-mini',
    })

    await vi.advanceTimersByTimeAsync(30_000)

    expect(handler.onDone).toHaveBeenCalledWith(5, 2, 's1', {servedModel: 'gpt-4o-mini'})
  })

  it('lets a finishing resume supply the served model, and drops any rate it still carries', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'hi'))
    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'hi there',
      done: true,
      error: '',
      tokensOut: 3,
      tokensIn: 1,
      servedModel: 'gpt-4o',
      ...({outputCostPerM: 10} as object),
    })

    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 2}))
    await vi.advanceTimersByTimeAsync(0)

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {servedModel: 'gpt-4o'})
  })
})

describe('error usage type', () => {
  it("types onError's usage as lib/chatEvent's ErrorUsage", () => {
    expectTypeOf<Parameters<StreamHandler['onError']>[3]>().toEqualTypeOf<ErrorUsage | undefined>()
  })
})

describe('fix_decision dispatch', () => {
  function fixDecisionEvent(streamId: string, data: Record<string, unknown>) {
    return {name: 'chat:event', data: {streamId, type: 'fix_decision', data}}
  }

  it('forwards per-item outcomes from a batch decision', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    capturedCb!(
      fixDecisionEvent('s1', {
        proposalId: 'batch-1',
        status: 'applied-unresolved',
        message: 'review',
        items: [
          {ruleId: 'r1', status: 'applied'},
          {ruleId: 'r2', status: 'error', message: 'still appears'},
        ],
      }),
    )

    expect(handler.onFixDecision).toHaveBeenCalledWith('batch-1', 'applied-unresolved', 'review', 's1', [
      {ruleId: 'r1', status: 'applied', message: undefined},
      {ruleId: 'r2', status: 'error', message: 'still appears'},
    ])
  })

  it('passes undefined items for a single-fix decision', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    capturedCb!(fixDecisionEvent('s1', {proposalId: 'p1', status: 'applied'}))

    expect(handler.onFixDecision).toHaveBeenCalledWith('p1', 'applied', undefined, 's1', undefined)
  })
})

describe('cancel', () => {
  it('tells the handler when the stop could not reach the server running the stream, and only then', async () => {
    const handler = {...makeHandler(), onStopUnreachable: vi.fn()}
    const {result} = renderHook(() => useStreamingMessage(handler))

    vi.mocked(chatApi.cancelStream).mockRejectedValueOnce(
      Object.assign(new Error('x'), {status: 503, code: 'REPLICA_UNREACHABLE'}),
    )
    result.current.cancel('s1')
    await vi.advanceTimersByTimeAsync(0)
    expect(handler.onStopUnreachable).toHaveBeenCalledWith('s1', expect.anything())

    vi.mocked(chatApi.cancelStream).mockRejectedValueOnce(
      Object.assign(new Error('x'), {status: 403, code: 'STREAM_NOT_FOUND'}),
    )
    result.current.cancel('s2')
    await vi.advanceTimersByTimeAsync(0)
    expect(handler.onStopUnreachable).toHaveBeenCalledTimes(1)
  })
})

describe('replay gap marker', () => {
  // The server could not replay what the reconnect missed: the chunk events of a running stream are among the gap, so
  // the stream is resumed by offset exactly as after a reconnect.
  function gap() {
    gapListeners.forEach(l => l())
  }

  it('resumes every registered stream by delta, as a reconnect does', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    capturedCb!(chunkEvent('s1', 'partial'))
    vi.mocked(chatApi.resumeStream).mockClear()
    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'partial more',
      done: false,
      error: '',
      tokensOut: 0,
      tokensIn: 0,
    })

    gap()
    await vi.advanceTimersByTimeAsync(0)

    expect(chatApi.resumeStream).toHaveBeenCalledTimes(1)
    expect(vi.mocked(chatApi.resumeStream).mock.calls[0]![0]).toBe('s1')
  })

  it('does nothing when no stream is registered', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    result.current.teardownStream?.('s1')
    vi.mocked(chatApi.resumeStream).mockClear()

    gap()
    await vi.advanceTimersByTimeAsync(0)
    expect(chatApi.resumeStream).not.toHaveBeenCalled()
  })
})

// R-streaming-5: every resume answer goes through one path, so its events reach onResumeState before done, error or
// the next stall timer, whichever call fetched it.
describe('resume answers apply their events', () => {
  const toolResult = {type: 'tool_result', data: {name: 'list_findings', label: 'Listing findings', ok: true}}

  it('applies the events of the full refetch when a live chunk raced a delta resume', async () => {
    const handler = makeHandler()
    let acc = 0
    handler.onChunk.mockImplementation((t: string) => (acc += t.length))
    handler.onReplace.mockImplementation((t: string) => (acc = t.length))
    handler.getAccLength.mockImplementation(() => acc)
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    const events = capturedCb!
    events(chunkEvent('s1', 'abc'))

    let answerDelta: (v: unknown) => void = () => {}
    vi.mocked(chatApi.resumeStream)
      .mockImplementationOnce(() => new Promise(r => (answerDelta = r)) as never)
      .mockResolvedValueOnce({
        text: 'abcdefgh',
        done: false,
        error: '',
        tokensOut: 0,
        tokensIn: 0,
        events: [toolResult],
      })
    setConnState('reconnecting')
    setConnState('open')
    await vi.advanceTimersByTimeAsync(0)
    events(chunkEvent('s1', 'de'))
    answerDelta({text: 'de', done: false, error: '', tokensOut: 0, tokensIn: 0, events: []})
    await vi.advanceTimersByTimeAsync(0)

    expect(chatApi.resumeStream).toHaveBeenLastCalledWith('s1', 0)
    expect(handler.onReplace).toHaveBeenLastCalledWith('abcdefgh', 's1')
    expect(handler.onResumeState).toHaveBeenLastCalledWith(
      [expect.objectContaining({kind: 'tool-result', name: 'list_findings'})],
      's1',
    )
  })

  it('applies the events a stall probe answer carries, before its done', async () => {
    const handler = makeHandler()
    const order: string[] = []
    handler.onResumeState.mockImplementation(() => order.push('events'))
    handler.onDone.mockImplementation(() => order.push('done'))
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')

    vi.mocked(chatApi.resumeStream).mockResolvedValue({
      text: 'all of it',
      done: true,
      error: '',
      tokensOut: 4,
      tokensIn: 2,
      events: [toolResult],
    })
    await vi.advanceTimersByTimeAsync(30_000)

    expect(handler.onResumeState).toHaveBeenCalledWith(
      [expect.objectContaining({kind: 'tool-result', name: 'list_findings'})],
      's1',
    )
    expect(order).toEqual(['events', 'done'])
  })
})

// R-streaming-c20: a stop keeps the stream's subscription until the server confirms it or the stream ends, so a stop
// that did not get through still hears the stream's end.
describe('cancel keeps listening until the stop is confirmed', () => {
  it('tears the subscription down only once the server confirms the stop', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    let confirm: () => void = () => {}
    vi.mocked(chatApi.cancelStream).mockImplementationOnce(() => new Promise<void>(r => (confirm = r)))

    result.current.cancel('s1')
    await vi.advanceTimersByTimeAsync(0)
    expect(capturedCb).not.toBeNull()

    confirm()
    await vi.advanceTimersByTimeAsync(0)
    expect(capturedCb).toBeNull()
  })

  it('offers the stop again after a 429, and keeps listening until the stream ends', async () => {
    const handler = {...makeHandler(), onStopUnreachable: vi.fn()}
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    const events = capturedCb!
    vi.mocked(chatApi.cancelStream).mockRejectedValueOnce(
      Object.assign(new Error('rate limit exceeded'), {status: 429, code: 'RATE_LIMITED'}),
    )

    result.current.cancel('s1')
    await vi.advanceTimersByTimeAsync(0)
    expect(handler.onStopUnreachable).toHaveBeenCalledWith('s1', expect.anything())
    expect(capturedCb).not.toBeNull()

    // A stopping stream is not probed: nothing is left to commit.
    await vi.advanceTimersByTimeAsync(60_000)
    expect(chatApi.resumeStream).not.toHaveBeenCalled()

    events({name: 'chat:event', data: {streamId: 's1', type: 'done', data: {tokensOut: 1, tokensIn: 1}}})
    expect(capturedCb).toBeNull()
  })
})

describe('unmount while a stop is under way', () => {
  it('does not send the stop again, and reports nothing once unmounted', async () => {
    const handler = {...makeHandler(), onStopUnreachable: vi.fn()}
    const {result, unmount} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1')
    let refuse: (err: unknown) => void = () => {}
    vi.mocked(chatApi.cancelStream).mockImplementationOnce(() => new Promise<void>((_r, reject) => (refuse = reject)))

    result.current.cancel('s1')
    unmount()
    refuse(Object.assign(new Error('rate limit exceeded'), {status: 429, code: 'RATE_LIMITED'}))
    await vi.advanceTimersByTimeAsync(0)

    expect(chatApi.cancelStream).toHaveBeenCalledTimes(1)
    expect(handler.onStopUnreachable).not.toHaveBeenCalled()
  })
})
