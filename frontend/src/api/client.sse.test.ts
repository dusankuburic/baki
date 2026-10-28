import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import type {BackendEvent} from '@/platform/types'

// The backend's address: the desktop shell respawns a dead sidecar on a new port with a new token (R-frontend-c7).
const backend = vi.hoisted(() => ({port: 9999, token: 'mock-token'}))
vi.mock('@/platform/adapters', () => ({
  createAdapter: () => ({
    getBackendConfig: vi.fn(async () => ({
      apiUrl: `http://localhost:${backend.port}`,
      token: backend.token,
      port: backend.port,
    })),
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
  backend.port = 9999
  backend.token = 'mock-token'
})

afterEach(async () => {
  // A stream a test left subscribed would keep its online/visibility listeners, and its reconnects, into the next.
  ;(await getClient()).resetEventStreamForTest()
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

describe('connectEvents never gives up while a listener exists (R-streaming-6)', () => {
  // /api/events answers 503 until the test lets it open; every other path answers 200.
  // A down /api/events either answers an error status (a proxy 5xx, too many connections) or is not reached at all.
  function mockOutage(failure: 'status' | 'network' = 'status') {
    const state = {eventsUp: false}
    const spy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (url, init) => {
      if (!String(url).endsWith('/api/events')) {
        return new Response('{}', {status: 200, headers: {'Content-Type': 'application/json'}})
      }
      if (!state.eventsUp) {
        if (failure === 'network') throw new TypeError('Failed to fetch')
        return new Response('service unavailable', {status: 503})
      }
      const body = new ReadableStream<Uint8Array>({
        start(ctl) {
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
    const eventFetches = () => spy.mock.calls.filter(([u]) => String(u).endsWith('/api/events')).length
    return {state, eventFetches}
  }

  async function exhaustOldBudget(failure: 'status' | 'network' = 'status') {
    const out = mockOutage(failure)
    const client = await getClient()
    await client.subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(181_000)
    expect(out.eventFetches()).toBe(11)
    return {...out, client}
  }

  it('keeps retrying at the 30 s ceiling and never reports idle', async () => {
    const {eventFetches, client} = await exhaustOldBudget()
    const seen: string[] = []
    client.subscribeConnectionState(s => seen.push(s))

    await vi.advanceTimersByTimeAsync(120_000)

    expect(eventFetches()).toBe(15)
    expect(client.getEventConnectionState()).not.toBe('idle')
    expect(seen).not.toContain('idle')
  })

  it('reconnects within 1 s of any successful API response after a network failure', async () => {
    const {state, eventFetches, client} = await exhaustOldBudget('network')
    state.eventsUp = true

    await client.request('/api/flows', {method: 'GET'})
    await vi.advanceTimersByTimeAsync(1_000)

    expect(eventFetches()).toBe(12)
    expect(client.getEventConnectionState()).toBe('open')
  })

  it('keeps the backoff after an error status from /api/events: other answers do not override it', async () => {
    const {eventFetches, client} = await exhaustOldBudget('status')

    await client.request('/api/flows', {method: 'GET'})
    await vi.advanceTimersByTimeAsync(1_000)
    expect(eventFetches()).toBe(11)

    await vi.advanceTimersByTimeAsync(29_000)
    expect(eventFetches()).toBe(12)
  })

  it('reconnects within 1 s when the tab becomes visible', async () => {
    const {eventFetches} = await exhaustOldBudget()

    Object.defineProperty(document, 'visibilityState', {value: 'visible', configurable: true})
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(1_000)

    expect(eventFetches()).toBe(12)
  })

  it('reconnects when the browser comes back online, every time, not only the first', async () => {
    const {eventFetches} = await exhaustOldBudget()

    window.dispatchEvent(new Event('online'))
    await vi.advanceTimersByTimeAsync(1_000)
    expect(eventFetches()).toBe(12)

    await vi.advanceTimersByTimeAsync(5_000)
    window.dispatchEvent(new Event('online'))
    await vi.advanceTimersByTimeAsync(0)
    expect(eventFetches()).toBe(13)
  })

  it('a new subscriber (a stream registering) reconnects at once instead of waiting out the backoff', async () => {
    const {eventFetches, client} = await exhaustOldBudget()

    await client.subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(1_000)

    expect(eventFetches()).toBe(12)
  })

  it('does not hammer the server: nudges never start more than one attempt a second', async () => {
    const {eventFetches, client} = await exhaustOldBudget('network')

    for (let i = 0; i < 10; i++) {
      await client.request(`/api/flows?i=${i}`, {method: 'GET'})
      await vi.advanceTimersByTimeAsync(100)
    }

    expect(eventFetches()).toBeLessThanOrEqual(13)
  })

  it('stops listening for online and visibility once torn down', async () => {
    const {eventFetches} = mockOutage()
    const client = await getClient()
    const unsub = await client.subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    unsub()
    await vi.advanceTimersByTimeAsync(5_000)
    expect(client.getEventConnectionState()).toBe('idle')
    const before = eventFetches()

    window.dispatchEvent(new Event('online'))
    document.dispatchEvent(new Event('visibilitychange'))
    await client.request('/api/flows', {method: 'GET'})
    await vi.advanceTimersByTimeAsync(60_000)

    expect(eventFetches()).toBe(before)
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

interface Call {
  url: string
  auth: string
  lastEventId: string | undefined
}

function mockRestartingBackend() {
  const calls: Call[] = []
  // Every events connection starts with a frame that carries an id: a reconnect has a cursor it could send.
  let frameId = 40
  vi.spyOn(globalThis, 'fetch').mockImplementation(async (url, init) => {
    const headers = (init?.headers ?? {}) as Record<string, string>
    calls.push({url: String(url), auth: headers.Authorization ?? '', lastEventId: headers['Last-Event-ID']})
    if (!String(url).endsWith('/api/events')) {
      return new Response('{"ok":true}', {status: 200, headers: {'Content-Type': 'application/json'}})
    }
    const encoder = new TextEncoder()
    const body = new ReadableStream<Uint8Array>({
      start(ctl) {
        ctl.enqueue(encoder.encode(`id: ${++frameId}\ndata: {"name":"x","data":{}}\n\n`))
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
  const events = () => calls.filter(c => c.url.endsWith('/api/events'))
  const others = () => calls.filter(c => !c.url.endsWith('/api/events'))
  return {calls, events, others}
}

function fakeShell() {
  let emit: ((e: BackendEvent) => void) | null = null
  const onBackendEvent = (cb: (e: BackendEvent) => void) => {
    emit = cb
    return () => {
      emit = null
    }
  }
  return {onBackendEvent, emit: (e: BackendEvent) => emit?.(e)}
}

describe('a backend that restarted (R-frontend-c7)', () => {
  beforeEach(() => {
    backend.port = 1111
    backend.token = 'tok-a'
  })

  it("after 'terminated' then 'ready' with a new port, requests and the events stream use the new url and token", async () => {
    const be = mockRestartingBackend()
    const shell = fakeShell()
    const client = await getClient()
    client.watchBackendRestarts(shell.onBackendEvent)
    await client.subscribeToEvents(() => {})
    await vi.advanceTimersByTimeAsync(0)
    await client.request('/api/flows', {method: 'GET'})
    expect(be.others()[0]).toMatchObject({url: 'http://localhost:1111/api/flows', auth: 'Bearer tok-a'})

    shell.emit({kind: 'terminated', message: 'exit code 1'})
    backend.port = 2222
    backend.token = 'tok-b'
    shell.emit({kind: 'ready', message: ''})
    await vi.advanceTimersByTimeAsync(0)

    // Same path inside the GET cache's lifetime: the answer of the old process is not reused either.
    await client.request('/api/flows', {method: 'GET'})
    expect(be.others().at(-1)).toMatchObject({url: 'http://localhost:2222/api/flows', auth: 'Bearer tok-b'})
    const ev = be.events()
    expect(ev).toHaveLength(2)
    // The new process has its own event log: the old one's cursor means nothing to it.
    expect(ev[1]).toMatchObject({url: 'http://localhost:2222/api/events', auth: 'Bearer tok-b', lastEventId: undefined})
    expect(client.getEventConnectionState()).toBe('open')
  })

  it("adopts the config a 'ready' event carries", async () => {
    const be = mockRestartingBackend()
    const shell = fakeShell()
    const client = await getClient()
    client.watchBackendRestarts(shell.onBackendEvent)
    await client.request('/api/a', {method: 'GET'})

    shell.emit({kind: 'error', message: 'spawn failed'})
    shell.emit({
      kind: 'ready',
      message: '',
      config: {apiUrl: 'http://localhost:3333', token: 'tok-c', port: 3333},
    })
    await client.request('/api/b', {method: 'GET'})

    expect(be.others().at(-1)).toMatchObject({url: 'http://localhost:3333/api/b', auth: 'Bearer tok-c'})
  })

  it("tells the restart subscribers once, and only for a 'ready' that follows a failure", async () => {
    mockRestartingBackend()
    const shell = fakeShell()
    const client = await getClient()
    const {onBackendRestarted} = await import('@/lib/backendRestart')
    const seen = vi.fn()
    onBackendRestarted(seen)
    client.watchBackendRestarts(shell.onBackendEvent)

    shell.emit({kind: 'ready', message: ''})
    expect(seen).not.toHaveBeenCalled()

    shell.emit({kind: 'terminated', message: ''})
    shell.emit({kind: 'ready', message: ''})
    shell.emit({kind: 'ready', message: ''})
    expect(seen).toHaveBeenCalledTimes(1)
  })
})
