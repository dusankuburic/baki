import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

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
        conns.push({ push: (frame: string) => ctl.enqueue(encoder.encode(frame)) })
        init?.signal?.addEventListener('abort', () => {
          try {
            ctl.error(new DOMException('The operation was aborted.', 'AbortError'))
          } catch {
            // already errored/closed
          }
        })
      },
    })
    return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
  })
  return { spy, conns }
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
    const { spy } = mockSSEFetch()
    const { subscribeToEvents, getEventConnectionState } = await getClient()

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
    const { spy, conns } = mockSSEFetch()
    const { subscribeToEvents, getEventConnectionState } = await getClient()

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
    const { spy, conns } = mockSSEFetch()
    const { subscribeToEvents } = await getClient()

    const listener = vi.fn()
    await subscribeToEvents(listener)
    await vi.advanceTimersByTimeAsync(0)

    await vi.advanceTimersByTimeAsync(40_000)
    conns[0].push('data: {"name":"chat:event","data":{"streamId":"s1"}}\n\n')
    await vi.advanceTimersByTimeAsync(40_000)

    expect(listener).toHaveBeenCalledWith({ name: 'chat:event', data: { streamId: 's1' } })
    expect(spy).toHaveBeenCalledTimes(1) // 40s + 40s straddling a read never trips the 45s watchdog
  })
})

describe('subscribeToEvents teardown grace period', () => {
  it('keeps the connection when a subscriber returns within the grace period', async () => {
    const { spy } = mockSSEFetch()
    const { subscribeToEvents, getEventConnectionState } = await getClient()

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
    const { spy } = mockSSEFetch()
    const { subscribeToEvents, getEventConnectionState } = await getClient()

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
