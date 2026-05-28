import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { collaborationService } from './CollaborationService'
import type { Envelope, ConnectionStatus } from './CollaborationService'

// Minimal WebSocket mock
class MockWebSocket {
  static OPEN = 1
  static CLOSING = 2
  static CLOSED = 3

  readyState = MockWebSocket.OPEN
  onopen: (() => void) | null = null
  onmessage: ((e: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null
  sent: string[] = []

  constructor(public url: string) {
    // Fire onopen asynchronously to mimic real WS
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
    this.onmessage?.({ data: JSON.stringify(env) })
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
  vi.stubGlobal('WebSocket', class extends MockWebSocket {
    constructor(url: string) {
      super(url)
      mockWs = this
    }
  })
  // Reset the service between tests by disconnecting
  collaborationService.disconnect()
})

afterEach(() => {
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
    expect(mockWs?.url).toBe(
      'ws://localhost:9000/ws?flowId=my-flow&ticket=ticket-123'
    )
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
      type: 'presence.join', flowId: 'flow-1', userId: 'u1',
      ts: new Date().toISOString(),
      payload: { userId: 'u1', displayName: 'Alice' },
    }
    mockWs!.receive(env)
    expect(received).toHaveLength(1)
    expect(received[0].type).toBe('presence.join')
  })

  it('send() writes JSON to the WebSocket when connected', async () => {
    collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
    await waitConnected()

    collaborationService.send({ type: 'ping' })
    expect(mockWs!.sent).toHaveLength(1)
    expect(JSON.parse(mockWs!.sent[0]).type).toBe('ping')
  })

  it('send() is a no-op when not connected', () => {
    // Don't connect — service is in disconnected state
    collaborationService.send({ type: 'ping' })
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
      type: 'ping', flowId: 'flow-1', ts: new Date().toISOString(),
    })
    expect(received).toHaveLength(0)
  })

  it('ignores malformed JSON from server', async () => {
    collaborationService.connect('flow-1', 'http://localhost:8080', ticketProvider)
    await waitConnected()

    // Should not throw
    mockWs!.onmessage?.({ data: 'not-json' })
    expect(collaborationService.getStatus()).toBe('connected')
  })
})
