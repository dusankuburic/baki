// Mirror of internal/websocket/events.go event types and payloads.

export type EventType =
  | 'presence.join'
  | 'presence.leave'
  | 'presence.update'
  | 'cursor.move'
  | 'block.update'
  | 'block.create'
  | 'block.delete'
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

export type ConnectionStatus = 'disconnected' | 'connecting' | 'connected' | 'error'

type EventHandler = (env: Envelope) => void
type StatusHandler = (s: ConnectionStatus) => void

const MAX_RECONNECT_DELAY_MS = 30_000

class CollaborationService {
  private ws: WebSocket | null = null
  private flowId: string | null = null
  private apiUrl: string | null = null
  private token: string | null = null
  private status: ConnectionStatus = 'disconnected'
  private handlers = new Set<EventHandler>()
  private statusHandlers = new Set<StatusHandler>()
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private reconnectAttempt = 0
  private shouldReconnect = false

  connect(flowId: string, apiUrl: string, token: string): void {
    this.disconnect()
    this.flowId = flowId
    this.apiUrl = apiUrl
    this.token = token
    this.shouldReconnect = true
    this.reconnectAttempt = 0
    this.openSocket()
  }

  disconnect(): void {
    this.shouldReconnect = false
    this.clearReconnectTimer()
    if (this.ws) {
      this.ws.close()
      this.ws = null
    }
    this.setStatus('disconnected')
  }

  send(env: Omit<Envelope, 'flowId' | 'userId' | 'ts'>): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(env))
    }
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

  private openSocket(): void {
    if (!this.flowId || !this.apiUrl || !this.token) return

    const wsBase = this.apiUrl
      .replace(/^http/, 'ws')
      .replace(/\/$/, '')
    const wsUrl = `${wsBase}/ws?flowId=${encodeURIComponent(this.flowId)}&token=${encodeURIComponent(this.token)}`

    this.setStatus('connecting')
    const ws = new WebSocket(wsUrl)
    this.ws = ws

    ws.onopen = () => {
      this.reconnectAttempt = 0
      this.setStatus('connected')
    }

    ws.onmessage = (event: MessageEvent) => {
      try {
        const env = JSON.parse(event.data as string) as Envelope
        this.handlers.forEach(h => h(env))
      } catch {
        // ignore malformed frames
      }
    }

    ws.onclose = () => {
      this.ws = null
      if (this.shouldReconnect) {
        this.scheduleReconnect()
      } else {
        this.setStatus('disconnected')
      }
    }

    ws.onerror = () => {
      // onclose fires immediately after onerror; reconnect logic lives there
      this.setStatus('error')
    }
  }

  private scheduleReconnect(): void {
    const delay = Math.min(500 * 2 ** this.reconnectAttempt, MAX_RECONNECT_DELAY_MS)
    this.reconnectAttempt++
    this.setStatus('connecting')
    this.reconnectTimer = setTimeout(() => {
      if (this.shouldReconnect) this.openSocket()
    }, delay)
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
  }

  private setStatus(s: ConnectionStatus): void {
    if (this.status === s) return
    this.status = s
    this.statusHandlers.forEach(h => h(s))
  }
}

export const collaborationService = new CollaborationService()
