
import {logger} from '@/lib/logger'

export type EventType =
  | 'presence.join'
  | 'presence.leave'
  | 'presence.update'
  | 'presence.sync'
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

export interface PresenceSyncPayload {
  users: PresencePayload[]
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

type TicketProvider = () => Promise<string>

const MAX_RECONNECT_DELAY_MS = 30_000

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
  private pingTimer: ReturnType<typeof setInterval> | null = null
  private inactivityTimer: ReturnType<typeof setTimeout> | null = null
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
      if (this.ws !== ws) return
      this.reconnectAttempt = 0
      this.setStatus('connected')
      this.startWatchdog()
    }

    ws.onmessage = (event: MessageEvent) => {
      if (this.ws !== ws) return
      this.armInactivity()
      try {
        const env = JSON.parse(event.data as string) as Envelope
        for (const handler of this.handlers) {
          try {
            handler(env)
          } catch (err) {
            logger.error('collaboration event handler threw (isolated)', err)
          }
        }
      } catch {
      }
    }

    ws.onclose = () => {
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
      if (this.ws !== ws) return
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
