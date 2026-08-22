// Mirror of internal/websocket/events.go event types and payloads.

import {logger} from '@/lib/logger'

export type EventType =
  | 'presence.join'
  | 'presence.leave'
  | 'presence.update'
  | 'cursor.move'
  | 'block.update'
  | 'block.create'
  | 'block.delete'
  | 'flow.changed'
  | 'error'
  | 'ping'
  | 'pong'

export interface Envelope {
  type: EventType
  flowId: string
  userId?: string
  ts: string
  payload?: unknown
}

export interface PresencePayload {
  userId: string
  displayName?: string
  avatarUrl?: string
  selectedBlockId?: string
}

export interface CursorPayload {
  userId: string
  blockId: string
  offset: number
}

export interface BlockPayload {
  blockId: string
  subflowId: string
  properties?: Record<string, unknown>
  version: number
}

export interface ErrorPayload {
  code: string
  message: string
}

export interface FlowChangedPayload {
  version: number
}

export type ConnectionStatus = 'disconnected' | 'connecting' | 'connected' | 'error'

type EventHandler = (env: Envelope) => void
type StatusHandler = (s: ConnectionStatus) => void

/**
 * Provides a fresh, single-use WebSocket connect ticket. Called once per
 * (re)connection attempt — tickets are short-lived and cannot be reused, so a
 * new one is fetched on every socket open.
 */
type TicketProvider = () => Promise<string>

const MAX_RECONNECT_DELAY_MS = 30_000

// Liveness watchdog for half-open sockets: server protocol pings never reach
// JS, so we ping app-level every WS_PING_INTERVAL_MS and any inbound frame
// re-arms the WS_INACTIVITY_TIMEOUT_MS timer.
const WS_PING_INTERVAL_MS = 20_000
const WS_INACTIVITY_TIMEOUT_MS = 45_000

class CollaborationService {
  private ws: WebSocket | null = null
  private flowId: string | null = null
  private apiUrl: string | null = null
  private getTicket: TicketProvider | null = null
  private status: ConnectionStatus = 'disconnected'
  private handlers = new Set<EventHandler>()
  private statusHandlers = new Set<StatusHandler>()
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private reconnectAttempt = 0
  private shouldReconnect = false
  // Liveness watchdog timers. See WS_PING_INTERVAL_MS / WS_INACTIVITY_TIMEOUT_MS.
  private pingTimer: ReturnType<typeof setInterval> | null = null
  private inactivityTimer: ReturnType<typeof setTimeout> | null = null
  // Bumped on every connect()/disconnect() so an in-flight ticket fetch from a
  // superseded attempt can detect it is stale and abort before opening a socket.
  private generation = 0

  connect(flowId: string, apiUrl: string, getTicket: TicketProvider): void {
    this.disconnect()
    this.flowId = flowId
    this.apiUrl = apiUrl
    this.getTicket = getTicket
    this.shouldReconnect = true
    this.reconnectAttempt = 0
    void this.openSocket()
  }

  disconnect(): void {
    this.shouldReconnect = false
    this.generation++
    this.clearReconnectTimer()
    this.clearWatchdog()
    if (this.ws) {
      this.ws.close()
      this.ws = null
    }
    this.setStatus('disconnected')
  }

  send(env: Omit<Envelope, 'flowId' | 'userId' | 'ts'>): boolean {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(env))
      return true
    }
    return false
  }

  subscribe(handler: EventHandler): () => void {
    this.handlers.add(handler)
    return () => this.handlers.delete(handler)
  }

  onStatusChange(handler: StatusHandler): () => void {
    this.statusHandlers.add(handler)
    return () => this.statusHandlers.delete(handler)
  }

  getStatus(): ConnectionStatus {
    return this.status
  }

  private async openSocket(): Promise<void> {
    if (!this.flowId || !this.apiUrl || !this.getTicket) return

    this.setStatus('connecting')

    // Exchange the access token for a short-lived ticket. Capture the current
    // generation so we can bail out if connect()/disconnect() ran while the
    // ticket request was in flight.
    const gen = this.generation
    let ticket: string
    try {
      ticket = await this.getTicket()
    } catch {
      if (gen === this.generation && this.shouldReconnect) this.scheduleReconnect()
      return
    }
    if (gen !== this.generation || !this.shouldReconnect) return

    const wsBase = this.apiUrl.replace(/^http/, 'ws').replace(/\/$/, '')
    const wsUrl = `${wsBase}/ws?flowId=${encodeURIComponent(this.flowId)}&ticket=${encodeURIComponent(ticket)}`

    const ws = new WebSocket(wsUrl)
    this.ws = ws

    ws.onopen = () => {
      if (this.ws !== ws) return // superseded socket
      this.reconnectAttempt = 0
      this.setStatus('connected')
      this.startWatchdog()
    }

    ws.onmessage = (event: MessageEvent) => {
      if (this.ws !== ws) return // superseded socket
      // Any inbound frame (real events or a pong reply) proves the socket is
      // alive — re-arm the inactivity watchdog before parsing so even a
      // malformed frame counts as liveness.
      this.armInactivity()
      try {
        const env = JSON.parse(event.data as string) as Envelope
        // Per-handler isolation (same class of fix as dispatchSafely in
        // api/client.ts): a throwing consumer must not abort delivery to the
        // handlers registered after it, nor be misclassified below as a
        // malformed frame.
        for (const handler of this.handlers) {
          try {
            handler(env)
          } catch (err) {
            logger.error('collaboration event handler threw (isolated)', err)
          }
        }
      } catch {
        // ignore malformed frames
      }
    }

    ws.onclose = () => {
      // A close from a superseded socket (e.g. one we already replaced during a
      // rapid reconnect) must not clobber the current connection's state.
      if (this.ws !== ws) return
      this.clearWatchdog()
      this.ws = null
      if (this.shouldReconnect) {
        this.scheduleReconnect()
      } else {
        this.setStatus('disconnected')
      }
    }

    ws.onerror = () => {
      if (this.ws !== ws) return // superseded socket
      // onclose fires immediately after onerror; reconnect logic lives there
      this.setStatus('error')
    }
  }

  private scheduleReconnect(): void {
    const delay = Math.min(500 * 2 ** this.reconnectAttempt, MAX_RECONNECT_DELAY_MS)
    this.reconnectAttempt++
    this.setStatus('connecting')
    this.reconnectTimer = setTimeout(() => {
      if (this.shouldReconnect) void this.openSocket()
    }, delay)
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
  }

  // Starts the liveness watchdog for the just-opened socket. Clearing stale
  // timers first guards against a superseded socket's onopen overwriting refs.
  private startWatchdog(): void {
    this.clearWatchdog()
    this.armInactivity()
    this.pingTimer = setInterval(() => {
      if (this.ws?.readyState === WebSocket.OPEN) {
        this.ws.send(JSON.stringify({type: 'ping'}))
      }
    }, WS_PING_INTERVAL_MS)
  }

  private armInactivity(): void {
    if (this.inactivityTimer) clearTimeout(this.inactivityTimer)
    this.inactivityTimer = setTimeout(() => {
      // Nothing arrived (not even a pong) within the window — presume the
      // socket is half-open (suspend/resume, silent network drop) and force it
      // closed so onclose runs the reconnect backoff.
      if (this.ws) this.ws.close()
    }, WS_INACTIVITY_TIMEOUT_MS)
  }

  private clearWatchdog(): void {
    if (this.pingTimer !== null) {
      clearInterval(this.pingTimer)
      this.pingTimer = null
    }
    if (this.inactivityTimer !== null) {
      clearTimeout(this.inactivityTimer)
      this.inactivityTimer = null
    }
  }

  private setStatus(s: ConnectionStatus): void {
    if (this.status === s) return
    this.status = s
    // Per-handler isolation: a throwing status listener must not propagate
    // into the caller (setStatus runs before reconnect timers / the watchdog).
    for (const handler of this.statusHandlers) {
      try {
        handler(s)
      } catch (err) {
        logger.error('collaboration status handler threw (isolated)', err)
      }
    }
  }
}

export const collaborationService = new CollaborationService()
