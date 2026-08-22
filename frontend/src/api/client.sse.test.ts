import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'

// SSE connection lifecycle tests for connectEvents/subscribeToEvents: the
// read-inactivity watchdog (a half-dead socket reconnects instead of hanging
// forever), heartbeat liveness, and the last-unsubscriber teardown grace
// period. Uses fake timers plus a fetch mock whose ReadableStream body is
// wired to the request's AbortSignal, mirroring real fetch abort semantics.

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

// mockSSEFetch replaces fetch with an SSE-like response: an open-ended body
// stream we can push frames into, which errors with AbortError when the
// caller aborts (as real fetch does).
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
            // already errored/closed
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

    // Total silence — no events, no pings. The watchdog aborts at 45s and the
    // backoff (1s after a fresh open) re-dials.
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

    // A ping every 20s for 2 minutes — well past the 45s inactivity timeout.
    for (let i = 0; i < 6; i++) {
      await vi.advanceTimersByTimeAsync(20_000)
      conns[0].push(': ping\n\n')
      await vi.advanceTimersByTimeAsync(0)
    }

    expect(spy).toHaveBeenCalledTimes(1) // no reconnect
    expect(getEventConnectionState()).toBe('open')
    expect(listener).not.toHaveBeenCalled() // comment frames are not events
  })

  it('still dispatches data events and resets the watchdog on them', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents} = await getClient()

    const listener = vi.fn()
    await subscribeToEvents(listener)
    await vi.advanceTimersByTimeAsync(0)

    await vi.advanceTimersByTimeAsync(40_000)
    conns[0].push('data: {"name":"chat:event","data":{"streamId":"s1"}}\n\n')
    await vi.advanceTimersByTimeAsync(40_000)

    expect(listener).toHaveBeenCalledWith({name: 'chat:event', data: {streamId: 's1'}})
    expect(spy).toHaveBeenCalledTimes(1) // 40s + 40s straddling a read never trips the 45s watchdog
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
    await vi.advanceTimersByTimeAsync(2_000) // within the 5s grace
    await subscribeToEvents(() => {})
    // Past the grace period (but under the 45s inactivity watchdog, since
    // this mock sends no heartbeats).
    await vi.advanceTimersByTimeAsync(40_000)

    expect(getEventConnectionState()).not.toBe('idle')
    expect(spy).toHaveBeenCalledTimes(1) // never re-dialed
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

    // An intentional teardown abort must not reconnect.
    await vi.advanceTimersByTimeAsync(120_000)
    expect(spy).toHaveBeenCalledTimes(1)
  })
})

// Server-close and backoff tests: the headline recovery paths that were
// previously only tested with a mocked connection (never driven through the
// real client.ts reconnect logic).
describe('connectEvents server-close and backoff', () => {
  it('reconnects when the server closes the stream (reader done)', async () => {
    // Mock a stream that immediately closes (done=true) — simulates a backend
    // restart mid-connection.
    vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
      const encoder = new TextEncoder()
      const body = new ReadableStream<Uint8Array>({
        start(ctl) {
          ctl.enqueue(encoder.encode(': ping\n\n'))
          ctl.close() // server closed the connection
        },
      })
      return new Response(body, {status: 200, headers: {'Content-Type': 'text/event-stream'}})
    })

    const {subscribeToEvents, getEventConnectionState} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)

    // Stream closed → scheduleReconnect fires after 1s backoff.
    await vi.advanceTimersByTimeAsync(1_000)
    expect(getEventConnectionState()).not.toBe('idle')
    expect(fetch).toHaveBeenCalledTimes(2) // re-dialed
  })

  it('uses exponential backoff (1s, 2s, 4s) on consecutive non-OK responses', async () => {
    // Every connection attempt returns 503 → each triggers a reconnect with
    // increasing backoff: 1s, 2s, 4s.
    vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
      return new Response('service unavailable', {status: 503})
    })

    const {subscribeToEvents} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(fetch).toHaveBeenCalledTimes(1)

    // First backoff: 1s
    await vi.advanceTimersByTimeAsync(1_000)
    expect(fetch).toHaveBeenCalledTimes(2)

    // Second backoff: 2s
    await vi.advanceTimersByTimeAsync(2_000)
    expect(fetch).toHaveBeenCalledTimes(3)

    // Third backoff: 4s
    await vi.advanceTimersByTimeAsync(4_000)
    expect(fetch).toHaveBeenCalledTimes(4)

    // The next backoff is 8s — verify it doesn't fire early.
    await vi.advanceTimersByTimeAsync(7_999)
    expect(fetch).toHaveBeenCalledTimes(4)
    await vi.advanceTimersByTimeAsync(1)
    expect(fetch).toHaveBeenCalledTimes(5)
  })
})

describe('connectEvents retry budget (bounded reconnects)', () => {
  // Every attempt fails → the loop must stop after SSE_MAX_RECONNECT_ATTEMPTS
  // consecutive failures, settle to 'idle', and recover on the browser
  // 'online' event.
  it('stops retrying after the budget is exhausted, then recovers on the online event', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
      return new Response('service unavailable', {status: 503})
    })

    const {subscribeToEvents, getEventConnectionState} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(fetch).toHaveBeenCalledTimes(1) // initial attempt

    // Burn through the entire backoff ladder (1+2+4+8+16+30×5 = 181s covers
    // 10 scheduled reconnects) plus the initial dial.
    await vi.advanceTimersByTimeAsync(181_000)
    const attemptsAfterBudget = (fetch as ReturnType<typeof vi.fn>).mock.calls.length
    expect(attemptsAfterBudget).toBe(11) // 1 initial + 10 retries — no more

    // Budget exhausted: settled to 'idle', and further time passes silently.
    expect(getEventConnectionState()).toBe('idle')
    await vi.advanceTimersByTimeAsync(120_000)
    expect(fetch).toHaveBeenCalledTimes(11)

    // Network comes back: the one-shot online listener restarts the loop.
    window.dispatchEvent(new Event('online'))
    await vi.advanceTimersByTimeAsync(0)
    expect(fetch).toHaveBeenCalledTimes(12)
    expect(['connecting', 'reconnecting']).toContain(getEventConnectionState())
  })
})
