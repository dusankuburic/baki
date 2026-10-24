import {describe, it, expect, expectTypeOf, vi, beforeEach, afterEach} from 'vitest'
import {renderHook} from '@testing-library/react'
import {useStreamingMessage, type StreamHandler} from './useStreamingMessage'
import {chatApi} from '@/api'
import type {ErrorUsage} from '@/lib/chatEvent'

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
    beginStream: vi.fn(),
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

    const p = result.current.registerStream('s1', false)
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

    const p = result.current.registerStream('s1', false)
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
    await result.current.registerStream('s1', false)

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
    await result.current.registerStream('s1', false)

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
    await result.current.registerStream('s1', false)

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
    await result.current.registerStream('s1', false)

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
    await result.current.registerStream('s1', false)

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
    await result.current.registerStream('s1', false)

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
    await result.current.registerStream('s1', false)

    for (let i = 0; i < 5; i++) {
      await vi.advanceTimersByTimeAsync(20_000)
      capturedCb!(chunkEvent('s1', 'x'))
    }

    expect(chatApi.resumeStream).not.toHaveBeenCalled()
  })

  it('counts a failed probe request as a miss, not an instant error', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1', false)

    vi.mocked(chatApi.resumeStream).mockRejectedValue(new Error('network'))
    await vi.advanceTimersByTimeAsync(30_000)

    expect(handler.onError).not.toHaveBeenCalled()

    await vi.advanceTimersByTimeAsync(30_000)
    await vi.advanceTimersByTimeAsync(30_000)
    expect(handler.onError).toHaveBeenCalledWith(expect.stringContaining('stalled'), 's1')
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
    await result.current.registerStream('s1', false)
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
    await result.current.registerStream('s1', false)
    result.current.confirmStream('s1')

    vi.mocked(chatApi.resumeStream).mockRejectedValue(goneError(403))
    await reconnect()

    expect(handler.onError).toHaveBeenCalledWith(expect.any(String), 's1', 'STREAM_INTERRUPTED')
  })

  it('does not report a stream whose create has not answered yet', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1', false)

    vi.mocked(chatApi.resumeStream).mockRejectedValue(goneError(404))
    await reconnect()

    expect(handler.onError).not.toHaveBeenCalled()
  })

  it('keeps waiting when the resume fails transiently', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1', false)
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
    await result.current.registerStream('s1', false)
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
    await result.current.registerStream('s1', false)

    capturedCb!(chunkEvent('s1', 'hi'))
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 1, servedModel: 'gpt-4o', ...RATES}))

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {servedModel: 'gpt-4o'})
  })

  it('passes the stored conversation version a done names, and the version the turn began from', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1', false)

    capturedCb!(chunkEvent('s1', 'hi'))
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 1, conversationVersion: 12, conversationBase: 10}))

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {conversationVersion: 12, conversationBase: 10})
  })

  it('keeps the conversation version through a chunk-mismatch resume that does not know it', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1', false)
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
    await result.current.registerStream('s1', false)

    capturedCb!(chunkEvent('s1', 'hi'))
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 1}))

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', undefined)
  })

  it('takes the served model from a chunk-mismatch resume and drops any rates', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1', false)
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
    await result.current.registerStream('s1', false)
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

  it('forwards the served model of a stream /begin reports as already finished', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    vi.mocked(chatApi.beginStream).mockResolvedValue({
      status: 'finished',
      text: 'done already',
      done: true,
      tokensOut: 2,
      tokensIn: 1,
      servedModel: 'gpt-4o',
    })

    await result.current.registerStream('s1', true)

    expect(handler.onReplace).toHaveBeenCalledWith('done already', 's1')
    expect(handler.onDone).toHaveBeenCalledWith(2, 1, 's1', {servedModel: 'gpt-4o'})
  })

  it('takes the served model a finished /begin reports', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    vi.mocked(chatApi.beginStream).mockResolvedValue({
      status: 'finished',
      text: 'done already',
      done: true,
      tokensOut: 2,
      tokensIn: 1,
      servedModel: 'gpt-4o',
      ...RATES,
    })

    await result.current.registerStream('s1', true)

    expect(handler.onDone).toHaveBeenCalledWith(2, 1, 's1', {servedModel: 'gpt-4o'})
  })

  it('lets a finishing resume supply the served model, and drops any rate it still carries', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1', false)
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
    await result.current.registerStream('s1', false)

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
    await result.current.registerStream('s1', false)

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
    expect(handler.onStopUnreachable).toHaveBeenCalledWith('s1')

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
    await result.current.registerStream('s1', false)
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
    await result.current.registerStream('s1', false)
    result.current.teardownStream?.('s1')
    vi.mocked(chatApi.resumeStream).mockClear()

    gap()
    await vi.advanceTimersByTimeAsync(0)
    expect(chatApi.resumeStream).not.toHaveBeenCalled()
  })
})
