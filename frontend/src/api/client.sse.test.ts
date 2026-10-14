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

describe('connectEvents event ids and Last-Event-ID', () => {
  // The headers each connection was opened with, in order.
  function headersOf(spy: ReturnType<typeof mockSSEFetch>['spy']): Record<string, string>[] {
    return spy.mock.calls.map(c => ({...((c[1] as RequestInit).headers as Record<string, string>)}))
  }

  // Lets the 45 s inactivity watchdog end the connection and the first backoff pass, so the client reconnects.
  async function reconnect() {
    await vi.advanceTimersByTimeAsync(45_000)
    await vi.advanceTimersByTimeAsync(1_000)
  }

  it('sends Last-Event-ID on a reconnect, from the id of the last frame it received', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents} = await getClient()
    const listener = vi.fn()
    await subscribeToEvents(listener)
    await vi.advanceTimersByTimeAsync(0)

    conns[0]!.push('id: 1700000000000-1\ndata: {"name":"flow:loaded","data":1}\n\n')
    conns[0]!.push('id: 1700000000000-2\ndata: {"name":"flow:loaded","data":2}\n\n')
    await vi.advanceTimersByTimeAsync(0)
    expect(listener).toHaveBeenCalledTimes(2)

    await reconnect()
    const sent = headersOf(spy)
    expect(sent).toHaveLength(2)
    expect(sent[0]).not.toHaveProperty('Last-Event-ID')
    expect(sent[1]!['Last-Event-ID']).toBe('1700000000000-2')
    expect(sent[1]!.Authorization).toBe('Bearer mock-token')
  })

  it('never sends the header to a server that sends no ids (an older release)', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    conns[0]!.push('data: {"name":"chat:event","data":{"type":"chunk"}}\n\n')
    conns[0]!.push(': ping\n\n')
    await vi.advanceTimersByTimeAsync(0)

    await reconnect()
    expect(headersOf(spy)[1]).not.toHaveProperty('Last-Event-ID')
  })

  it('keeps the last id across frames without one (chunks, heartbeats) and across a split frame', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents} = await getClient()
    const listener = vi.fn()
    await subscribeToEvents(listener)
    await vi.advanceTimersByTimeAsync(0)

    conns[0]!.push('id:5-1\ndata: {"name":"a","data":1}\n\n')
    conns[0]!.push('data: {"name":"chat:event","data":"chunk"}\n\n')
    // The next frame's id and data arrive in separate reads.
    conns[0]!.push('id: 5-2\nda')
    await vi.advanceTimersByTimeAsync(0)
    conns[0]!.push('ta: {"name":"b","data":2}\n\n')
    await vi.advanceTimersByTimeAsync(0)
    expect(listener).toHaveBeenCalledTimes(3)

    await reconnect()
    expect(headersOf(spy)[1]!['Last-Event-ID']).toBe('5-2')
  })

  it('applies an id only when its data line arrives: a frame cut off by the disconnect does not advance it', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)

    conns[0]!.push('id: 9-1\ndata: {"name":"a","data":1}\n\n')
    conns[0]!.push('id: 9-2\n') // the connection dies before this event's data
    await vi.advanceTimersByTimeAsync(0)

    await reconnect()
    expect(headersOf(spy)[1]!['Last-Event-ID']).toBe('9-1')
  })

  it('ignores an id that cannot be one (empty, with a NUL, or absurdly long)', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)

    conns[0]!.push('id: 7-1\ndata: {"name":"a","data":1}\n\n')
    conns[0]!.push('id:\ndata: {"name":"a","data":2}\n\n')
    conns[0]!.push('id: 7\u0000-2\ndata: {"name":"a","data":3}\n\n')
    conns[0]!.push(`id: ${'9'.repeat(200)}\ndata: {"name":"a","data":4}\n\n`)
    await vi.advanceTimersByTimeAsync(0)

    await reconnect()
    expect(headersOf(spy)[1]!['Last-Event-ID']).toBe('7-1')
  })

  it('forgets the id when the stream is torn down: the next session starts without a replay', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents} = await getClient()
    const unsubscribe = await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    conns[0]!.push('id: 3-1\ndata: {"name":"a","data":1}\n\n')
    await vi.advanceTimersByTimeAsync(0)

    unsubscribe()
    await vi.advanceTimersByTimeAsync(6_000) // past the teardown grace
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(headersOf(spy)[1]).not.toHaveProperty('Last-Event-ID')
  })

  it('forgets the id on sign-out', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents, setSessionToken} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    conns[0]!.push('id: 4-1\ndata: {"name":"a","data":1}\n\n')
    await vi.advanceTimersByTimeAsync(0)

    setSessionToken(null)
    await reconnect()
    expect(headersOf(spy)[1]).not.toHaveProperty('Last-Event-ID')
  })
})

describe('connectEvents replay gap marker', () => {
  // The server could not replay everything the reconnect missed (more than the log carries, or Redis could not be read)
  // and says so instead of leaving a silent hole between the replayed and the live frames.
  const GAP = 'id: 1700000000000-9\ndata: {"name":"replay:gap","data":{}}\n\n'

  it('tells the gap subscribers once, and does not hand the marker to the event listeners', async () => {
    const {conns} = mockSSEFetch()
    const {subscribeToEvents, subscribeEventGap} = await getClient()
    const listener = vi.fn()
    const onGap = vi.fn()
    subscribeEventGap(onGap)
    await subscribeToEvents(listener)
    await vi.advanceTimersByTimeAsync(0)

    conns[0]!.push(GAP)
    conns[0]!.push('id: 1700000000000-10\ndata: {"name":"flow:loaded","data":1}\n\n')
    await vi.advanceTimersByTimeAsync(0)

    expect(onGap).toHaveBeenCalledTimes(1)
    expect(listener).toHaveBeenCalledTimes(1)
    expect(listener).toHaveBeenCalledWith({name: 'flow:loaded', data: 1})
  })

  it('moves the reconnect cursor to the marker id, so the next reconnect does not replay the same gap', async () => {
    const {spy, conns} = mockSSEFetch()
    const {subscribeToEvents} = await getClient()
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    conns[0]!.push('id: 1700000000000-1\ndata: {"name":"flow:loaded","data":1}\n\n')
    conns[0]!.push(GAP)
    await vi.advanceTimersByTimeAsync(0)

    await vi.advanceTimersByTimeAsync(45_000)
    await vi.advanceTimersByTimeAsync(1_000)
    const sent = spy.mock.calls.map(c => ({...((c[1] as RequestInit).headers as Record<string, string>)}))
    expect(sent[1]!['Last-Event-ID']).toBe('1700000000000-9')
  })

  it('stops notifying an unsubscribed gap subscriber', async () => {
    const {conns} = mockSSEFetch()
    const {subscribeToEvents, subscribeEventGap} = await getClient()
    const onGap = vi.fn()
    const off = subscribeEventGap(onGap)
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    off()
    conns[0]!.push(GAP)
    await vi.advanceTimersByTimeAsync(0)
    expect(onGap).not.toHaveBeenCalled()
  })

  it('isolates a gap subscriber that throws', async () => {
    const {conns} = mockSSEFetch()
    const {subscribeToEvents, subscribeEventGap} = await getClient()
    const second = vi.fn()
    subscribeEventGap(() => {
      throw new Error('boom')
    })
    subscribeEventGap(second)
    await subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    conns[0]!.push(GAP)
    await vi.advanceTimersByTimeAsync(0)
    expect(second).toHaveBeenCalledTimes(1)
  })
})
