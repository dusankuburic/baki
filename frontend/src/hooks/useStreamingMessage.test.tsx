import {describe, it, expect, expectTypeOf, vi, beforeEach, afterEach} from 'vitest'
import {renderHook} from '@testing-library/react'
import {useStreamingMessage, type StreamHandler} from './useStreamingMessage'
import {chatApi} from '@/api'
import type {ErrorUsage} from '@/lib/chatEvent'

// Failure-path tests for useStreamingMessage: the bounded open-wait (a send
// against an unreachable backend errors instead of hanging forever) and the
// stall probe (a dropped done/error event — or a backend worker that died
// without emitting anything — is recovered via resume or errored out).

type EventCb = (ev: {name: string; data: unknown}) => void
type StateCb = (state: string) => void

let connState = 'open'
let capturedCb: EventCb | null = null
const stateListeners = new Set<StateCb>()

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
  vi.clearAllMocks()
  // Default: resume finds a live stream with nothing buffered (individual
  // tests override). The reconnect listener delta-resumes on any
  // connecting→open transition, so this must always return a promise.
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

    // The dead registration was torn down: its SSE listener is unsubscribed,
    // so a late event can't reach the handlers.
    expect(capturedCb).toBeNull()
    expect(handler.onError).not.toHaveBeenCalled() // caller owns error display
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

    // done was dropped; the probe fetches the authoritative buffer.
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
    // No served model on this resume: the meta argument is undefined.
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

    // The failure's code travels with it, so a recovered error gets the same
    // handling as a live one.
    // F-15: a fourth usage argument travels even when the failure carried
    // none (undefined = unbilled).
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

    // Five minutes of server-side silence (an approval wait, a slow
    // re-analysis) with the backend's keepalive every 20s: never probed,
    // never declared stalled, and a heartbeat is not text.
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

    // Backend has the stream but it never progresses and never terminates
    // (e.g. the worker died without recording done or error).
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

    // Text grew past what the client holds: not a stall, misses reset.
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

// Regression: the live SSE dispatch must forward a fix_decision's per-item
// outcomes. parseChatEvent has always produced `items[]` for batch decisions and
// useChatStreamEngine.onFixDecision has always accepted them, but the
// StreamHandler signature stopped at 4 parameters and the dispatch dropped
// `parsed.items` on the floor. A batch where 3 of 5 fixes applied and 2 errored
// therefore rendered all 5 rows identically — until an SSE reconnect, whose
// separate onResumeState path DID honour the items, making the bug intermittent.
//
// These assertions run against the real dispatcher (not a hand-rolled handler
// type), which is the whole point: the previous test for this behaviour declared
// its own 5-arg interface and invoked it directly, so it exercised a contract no
// production code implemented.
describe('done meta (served model + billed rates)', () => {
  function doneEvent(streamId: string, data: Record<string, unknown>) {
    return {name: 'chat:event', data: {streamId, type: 'done', data}}
  }
  const RATES = {inputCostPerM: 2.5, outputCostPerM: 10}

  it('passes the served model and rates of a live done to the handler', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1', false)

    capturedCb!(chunkEvent('s1', 'hi'))
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 1, servedModel: 'gpt-4o', ...RATES}))

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {servedModel: 'gpt-4o', ...RATES})
  })

  it('passes no meta for a plain done', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1', false)

    capturedCb!(chunkEvent('s1', 'hi'))
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 1}))

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', undefined)
  })

  it("keeps the done event's rates through a chunk-mismatch resume and takes the served model from it", async () => {
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

    // Two chunks announced, one received: the finish goes through resume.
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 2, ...RATES}))
    await vi.advanceTimersByTimeAsync(0)

    expect(chatApi.resumeStream).toHaveBeenCalledWith('s1', 0)
    expect(handler.onReplace).toHaveBeenCalledWith('hi there', 's1')
    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {servedModel: 'gpt-4o', ...RATES})
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

  // R-3: resume and /begin now carry the billed rates like the done event.
  it('takes the billed rates a finished /begin reports', async () => {
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

    expect(handler.onDone).toHaveBeenCalledWith(2, 1, 's1', {servedModel: 'gpt-4o', ...RATES})
  })

  it('lets a finishing resume supply the rates, and ignores non-numeric ones', async () => {
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
      inputCostPerM: 3,
      outputCostPerM: 'x' as unknown as number,
    })

    // The done event carried no rates; the resume that finishes it does.
    capturedCb!(doneEvent('s1', {tokensOut: 3, tokensIn: 1, chunks: 2}))
    await vi.advanceTimersByTimeAsync(0)

    expect(handler.onDone).toHaveBeenCalledWith(3, 1, 's1', {servedModel: 'gpt-4o', inputCostPerM: 3})
  })
})

// One ErrorUsage shape (lib/chatEvent) for the parsed error event and the
// handler's usage argument: separate copies could drift apart unnoticed.
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
