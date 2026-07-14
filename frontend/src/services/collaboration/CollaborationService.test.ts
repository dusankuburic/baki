import {describe, it, expect, vi, beforeEach, afterEach} from 'vitest'
import {collaborationService} from './CollaborationService'
import type {Envelope, ConnectionStatus} from './CollaborationService'

// Minimal WebSocket mock
class MockWebSocket {
  static OPEN = 1
  static CLOSING = 2
  static CLOSED = 3
  static onCreate: ((instance: MockWebSocket) => void) | null = null

  readyState = MockWebSocket.OPEN
  onopen: (() => void) | null = null
  onmessage: ((e: {data: string}) => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null
  sent: string[] = []

  constructor(public url: string) {
    MockWebSocket.onCreate?.(this)
    setTimeout(() => this.onopen?.(), 0)
  }

  send(data: string) {
    this.sent.push(data)
  }

  close() {
    this.readyState = MockWebSocket.CLOSED
    setTimeout(() => this.onclose?.(), 0)
  }

  // Test helper: push a message from the server
  receive(env: Envelope) {
    this.onmessage?.({data: JSON.stringify(env)})
  }
}

let mockWs: MockWebSocket | null = null

// A ticket provider stand-in. The socket is opened only after this resolves, so
// tests await a macrotask before inspecting the created WebSocket.
const ticketProvider = () => Promise.resolve('ticket-123')

// Wait long enough for the awaited ticket fetch + the mock's async onopen.
const flush = () => new Promise(r => setTimeout(r, 10))

// Deterministically wait until the socket reports connected (the ticket fetch
// and onopen are both async, so a fixed delay can flake under load).
async function waitConnected() {
  for (let i = 0; i < 100 && collaborationService.getStatus() !== 'connected'; i++) {
    await new Promise(r => setTimeout(r, 2))
  }
}

beforeEach(() => {
  mockWs = null
  MockWebSocket.onCreate = instance => {
    mockWs = instance
  }
  vi.stubGlobal('WebSocket', MockWebSocket)
  // Reset the service between tests by disconnecting
  collaborationService.disconnect()
})

afterEach(() => {
  MockWebSocket.onCreate = null
  collaborationService.disconnect()
  vi.unstubAllGlobals()
})

describe('CollaborationService', () => {
  it('transitions to connecting then connected on open', async () => {
    const statuses: ConnectionStatus[] = []
    collaborationService.onStatusChange(s => statuses.push(s))

    collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
    expect(statuses).toContain('connecting')

    await waitConnected()
    expect(statuses.at(-1)).toBe('connected')
  })

  it('builds the WebSocket URL with a ticket (not the token)', async () => {
    collaborationService.connect('my-flow', 'http://localhost:9000', ticketProvider)
    await waitConnected()
    expect(mockWs?.url).toBe('ws://localhost:9000/ws?flowId=my-flow&ticket=ticket-123')
    // The long-lived access token must never appear in the URL.
    expect(mockWs?.url).not.toContain('token=')
  })

  it('replaces https with wss in the URL', async () => {
    collaborationService.connect('f1', 'https://example.com', ticketProvider)
    await waitConnected()
    expect(mockWs?.url.startsWith('wss://')).toBe(true)
  })

  it('fetches a fresh ticket on each connection attempt', async () => {
    const getTicket = vi.fn(() => Promise.resolve('t-abc'))
    collaborationService.connect('flow-1', 'http://localhost:8080', getTicket)
    await waitConnected()
    expect(getTicket).toHaveBeenCalledTimes(1)
  })

  it('does not open a socket if the ticket fetch fails', async () => {
    const getTicket = vi.fn(() => Promise.reject(new Error('401')))
    collaborationService.connect('flow-1', 'http://localhost:8080', getTicket)
    await flush()
    expect(mockWs).toBeNull()
  })

  it('delivers incoming envelopes to subscribers', async () => {
    const received: Envelope[] = []
    collaborationService.subscribe(e => received.push(e))
    collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
    await waitConnected()

    const env: Envelope = {
      type: 'presence.join',
      flowId: 'flow-1',
      userId: 'u1',
      ts: new Date().toISOString(),
      payload: {userId: 'u1', displayName: 'Alice'},
    }
    mockWs!.receive(env)
    expect(received).toHaveLength(1)
    expect(received[0].type).toBe('presence.join')
  })

  it('send() writes JSON to the WebSocket when connected', async () => {
    collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
    await waitConnected()

    collaborationService.send({type: 'ping'})
    expect(mockWs!.sent).toHaveLength(1)
    expect(JSON.parse(mockWs!.sent[0]).type).toBe('ping')
  })

  it('send() is a no-op when not connected', () => {
    // Don't connect — service is in disconnected state
    collaborationService.send({type: 'ping'})
    expect(mockWs).toBeNull()
  })

  it('transitions to disconnected on explicit disconnect()', async () => {
    const statuses: ConnectionStatus[] = []
    collaborationService.onStatusChange(s => statuses.push(s))

    collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
    await waitConnected()

    collaborationService.disconnect()
    await flush()

    expect(statuses.at(-1)).toBe('disconnected')
  })

  it('does not reconnect after explicit disconnect()', async () => {
    collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
    await waitConnected()
    collaborationService.disconnect()
    await flush()

    const wsCountBefore = mockWs ? 1 : 0
    // No new WebSocket should be created
    await new Promise(r => setTimeout(r, 50))
    expect(mockWs ? 1 : 0).toBe(wsCountBefore)
  })

  it('getStatus() reflects current connection state', async () => {
    expect(collaborationService.getStatus()).toBe('disconnected')
    collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
    expect(collaborationService.getStatus()).toBe('connecting')
    await waitConnected()
    expect(collaborationService.getStatus()).toBe('connected')
  })

  it('unsubscribing stops event delivery', async () => {
    const received: Envelope[] = []
    const unsub = collaborationService.subscribe(e => received.push(e))

    collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
    await waitConnected()

    unsub()
    mockWs!.receive({
      type: 'ping',
      flowId: 'flow-1',
      ts: new Date().toISOString(),
    })
    expect(received).toHaveLength(0)
  })

  it('ignores malformed JSON from server', async () => {
    collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
    await waitConnected()

    // Should not throw
    mockWs!.onmessage?.({data: 'not-json'})
    expect(collaborationService.getStatus()).toBe('connected')
  })

  describe('liveness watchdog', () => {
    // Must mirror the private constants in CollaborationService.ts.
    const WS_PING_INTERVAL_MS = 20_000
    const WS_INACTIVITY_TIMEOUT_MS = 45_000

    beforeEach(() => {
      vi.useFakeTimers()
    })

    afterEach(() => {
      vi.useRealTimers()
    })

    it('sends an app-level ping every ping interval', async () => {
      collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
      // Flush the awaited ticket fetch + the mock's deferred onopen.
      await vi.advanceTimersByTimeAsync(10)
      mockWs!.sent = []

      await vi.advanceTimersByTimeAsync(WS_PING_INTERVAL_MS)
      expect(mockWs!.sent.some(s => JSON.parse(s).type === 'ping')).toBe(true)

      // A second interval elapses → a second ping.
      await vi.advanceTimersByTimeAsync(WS_PING_INTERVAL_MS)
      expect(mockWs!.sent.filter(s => JSON.parse(s).type === 'ping')).toHaveLength(2)
    })

    it('force-closes the socket after the inactivity window with no traffic', async () => {
      collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
      await vi.advanceTimersByTimeAsync(10)
      expect(collaborationService.getStatus()).toBe('connected')

      // No inbound frames arrive — advance past the inactivity window.
      await vi.advanceTimersByTimeAsync(WS_INACTIVITY_TIMEOUT_MS)
      // close() defers onclose; flush it so the reconnect path runs.
      await vi.advanceTimersByTimeAsync(10)
      expect(collaborationService.getStatus()).not.toBe('connected')
    })

    it('re-arms the inactivity timer on any inbound frame', async () => {
      collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
      await vi.advanceTimersByTimeAsync(10)

      // Move most of the way to the deadline, then receive a pong (the reply
      // to our ping) which must reset the watchdog.
      await vi.advanceTimersByTimeAsync(WS_INACTIVITY_TIMEOUT_MS - 5_000)
      mockWs!.receive({type: 'pong', flowId: 'flow-1', ts: new Date().toISOString()})
      await vi.advanceTimersByTimeAsync(WS_INACTIVITY_TIMEOUT_MS - 5_000)
      // Still connected — the pong pushed the deadline out.
      expect(collaborationService.getStatus()).toBe('connected')
    })

    it('clears the watchdog on explicit disconnect', async () => {
      collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
      await vi.advanceTimersByTimeAsync(10)
      mockWs!.sent = []

      collaborationService.disconnect()
      await vi.advanceTimersByTimeAsync(WS_PING_INTERVAL_MS * 2)
      // No pings should fire after teardown.
      expect(mockWs!.sent.some(s => JSON.parse(s).type === 'ping')).toBe(false)
    })
  })
})
