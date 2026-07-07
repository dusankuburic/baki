import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {renderHook} from '@testing-library/react'
import {useStreamingMessage} from './useStreamingMessage'
import {chatApi} from '@/api'

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
    return () => { capturedCb = null }
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
    await expect(p).resolves.toBeUndefined()
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
    vi.mocked(chatApi.resumeStream).mockResolvedValue({text: 'hello world', done: true, error: '', tokensOut: 5, tokensIn: 2})
    await vi.advanceTimersByTimeAsync(30_000)

    expect(chatApi.resumeStream).toHaveBeenCalledWith('s1', 0)
    expect(handler.onReplace).toHaveBeenCalledWith('hello world', 's1')
    expect(handler.onDone).toHaveBeenCalledWith(5, 2, 's1')
  })

  it('surfaces a dropped error event via resume', async () => {
    const handler = makeHandler()
    const {result} = renderHook(() => useStreamingMessage(handler))
    await result.current.registerStream('s1', false)

    vi.mocked(chatApi.resumeStream).mockResolvedValue({text: '', done: false, error: 'provider exploded', tokensOut: 0, tokensIn: 0})
    await vi.advanceTimersByTimeAsync(30_000)

    expect(handler.onError).toHaveBeenCalledWith('provider exploded', 's1')
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
    vi.mocked(chatApi.resumeStream).mockResolvedValue({text: 'partial', done: false, error: '', tokensOut: 0, tokensIn: 0})
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
