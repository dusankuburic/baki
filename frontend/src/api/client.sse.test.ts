import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'

vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn().mockResolvedValue({
      apiUrl: 'http://localhost:9999',
      token: 'mock-token',
      port: 9999,
    }),
  }),
}))

interface SSEConn {
  push: (frame: string) => void
}

function mockSSEFetch() {
  const conns: SSEConn[] = []
  const spy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (_url, init) => {
    const encoder = new TextEncoder()
    const body = new ReadableStream<Uint8Array>({
      start(ctl) {
        conns.push({push: (frame: string) => ctl.enqueue(encoder.encode(frame))})
        init?.signal?.addEventListener('abort', () => {
          try {
            ctl.error(new DOMException('The operation was aborted.', 'AbortError'))
          } catch {
          }
        })
      },
    })
    return new Response(body, {status: 200, headers: {'Content-Type': 'text/event-stream'}})
  })
  return {spy, conns}
}

async function getClient() {
  return import('./client')
}

beforeEach(() => {
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.resetModules()
})

describe('connectEvents inactivity watchdog', () => {
  it('reconnects when nothing arrives for the inactivity timeout', async () => {
    const {spy} = mockSSEFetch()
    const {subscribeToEvents, getEventConnectionState} = await getClient()

    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(spy).toHaveBeenCalledTimes(1)
    expect(getEventConnectionState()).toBe('open')

    await vi.advanceTimersByTimeAsync(45_000)
    expect(getEventConnectionState()).toBe('reconnecting')
    await vi.advanceTimersByTimeAsync(1_000)
    expect(spy).toHaveBeenCalledTimes(2)
  })

  it('heartbeat comment frames keep the connection alive and never reach listeners', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents, getEventConnectionState} = await getClient()

    const listener = vi.fn()
    await subscribeToEvents(listener)
    await vi.advanceTimersByTimeAsync(0)
    expect(spy).toHaveBeenCalledTimes(1)

    for (let i = 0; i < 6; i++) {
      await vi.advanceTimersByTimeAsync(20_000)
      conns[0]!.push(': ping\n\n')
      await vi.advanceTimersByTimeAsync(0)
    }

    expect(spy).toHaveBeenCalledTimes(1)
    expect(getEventConnectionState()).toBe('open')
    expect(listener).not.toHaveBeenCalled()
  })

  it('still dispatches data events and resets the watchdog on them', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents} = await getClient()

    const listener = vi.fn()
    await subscribeToEvents(listener)
    await vi.advanceTimersByTimeAsync(0)

    await vi.advanceTimersByTimeAsync(40_000)
    conns[0]!.push('data: {"name":"chat:event","data":{"streamId":"s1"}}\n\n')
    await vi.advanceTimersByTimeAsync(40_000)

    expect(listener).toHaveBeenCalledWith({name: 'chat:event', data: {streamId: 's1'}})
    expect(spy).toHaveBeenCalledTimes(1)
  })
})

describe('subscribeToEvents teardown grace period', () => {
  it('keeps the connection when a subscriber returns within the grace period', async () => {
    const {spy} = mockSSEFetch()
    const {subscribeToEvents, getEventConnectionState} = await getClient()

    const unsub = await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(spy).toHaveBeenCalledTimes(1)

    unsub()
    await vi.advanceTimersByTimeAsync(2_000)
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(40_000)

    expect(getEventConnectionState()).not.toBe('idle')
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('tears down for good once the grace period elapses with no subscribers', async () => {
    const {spy} = mockSSEFetch()
    const {subscribeToEvents, getEventConnectionState} = await getClient()

    const unsub = await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(spy).toHaveBeenCalledTimes(1)

    unsub()
    await vi.advanceTimersByTimeAsync(5_000)
    expect(getEventConnectionState()).toBe('idle')

    await vi.advanceTimersByTimeAsync(120_000)
    expect(spy).toHaveBeenCalledTimes(1)
  })
})

describe('connectEvents server-close and backoff', () => {
  it('reconnects when the server closes the stream (reader done)', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
      const encoder = new TextEncoder()
      const body = new ReadableStream<Uint8Array>({
        start(ctl) {
          ctl.enqueue(encoder.encode(': ping\n\n'))
          ctl.close()
        },
      })
      return new Response(body, {status: 200, headers: {'Content-Type': 'text/event-stream'}})
    })

    const {subscribeToEvents, getEventConnectionState} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)

    await vi.advanceTimersByTimeAsync(1_000)
    expect(getEventConnectionState()).not.toBe('idle')
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('uses exponential backoff (1s, 2s, 4s) on consecutive non-OK responses', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
      return new Response('service unavailable', {status: 503})
    })

    const {subscribeToEvents} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(fetch).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(1_000)
    expect(fetch).toHaveBeenCalledTimes(2)

    await vi.advanceTimersByTimeAsync(2_000)
    expect(fetch).toHaveBeenCalledTimes(3)

    await vi.advanceTimersByTimeAsync(4_000)
    expect(fetch).toHaveBeenCalledTimes(4)

    await vi.advanceTimersByTimeAsync(7_999)
    expect(fetch).toHaveBeenCalledTimes(4)
    await vi.advanceTimersByTimeAsync(1)
    expect(fetch).toHaveBeenCalledTimes(5)
  })
})

describe('connectEvents retry budget (bounded reconnects)', () => {
  it('stops retrying after the budget is exhausted, then recovers on the online event', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
      return new Response('service unavailable', {status: 503})
    })

    const {subscribeToEvents, getEventConnectionState} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(fetch).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(181_000)
    const attemptsAfterBudget = (fetch as ReturnType<typeof vi.fn>).mock.calls.length
    expect(attemptsAfterBudget).toBe(11)

    expect(getEventConnectionState()).toBe('idle')
    await vi.advanceTimersByTimeAsync(120_000)
    expect(fetch).toHaveBeenCalledTimes(11)

    window.dispatchEvent(new Event('online'))
    await vi.advanceTimersByTimeAsync(0)
    expect(fetch).toHaveBeenCalledTimes(12)
    expect(['connecting', 'reconnecting']).toContain(getEventConnectionState())
  })
})
