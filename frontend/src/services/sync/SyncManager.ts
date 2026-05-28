import {
  collaborationService,
  type Envelope,
  type ConnectionStatus,
} from '@/services/collaboration/CollaborationService'

export interface PendingOp {
  id: string
  env: Omit<Envelope, 'flowId' | 'userId' | 'ts'>
  queuedAt: number
}

type QueueChangeHandler = (queue: readonly PendingOp[]) => void

// Drop ops that have been waiting longer than this (ms) without a connection.
const MAX_AGE_MS = 60_000

class SyncManager {
  private queue: PendingOp[] = []
  private changeHandlers = new Set<QueueChangeHandler>()
  private unsubscribeStatus: (() => void) | null = null
  private counter = 0

  start(): void {
    if (this.unsubscribeStatus) return
    this.unsubscribeStatus = collaborationService.onStatusChange(this.handleStatusChange)
  }

  stop(): void {
    this.unsubscribeStatus?.()
    this.unsubscribeStatus = null
    this.queue = []
    this.notifyChange()
  }

  // Enqueue an op for delivery. If already connected, sends immediately.
  // Returns the assigned op id.
  enqueue(env: Omit<Envelope, 'flowId' | 'userId' | 'ts'>): string {
    const id = `op-${++this.counter}-${Date.now()}`
    if (collaborationService.getStatus() === 'connected') {
      collaborationService.send(env)
      return id
    }
    this.queue.push({ id, env, queuedAt: Date.now() })
    this.notifyChange()
    return id
  }

  getQueue(): readonly PendingOp[] {
    return this.queue
  }

  onQueueChange(handler: QueueChangeHandler): () => void {
    this.changeHandlers.add(handler)
    return () => this.changeHandlers.delete(handler)
  }

  private handleStatusChange = (status: ConnectionStatus): void => {
    if (status === 'connected') this.flush()
  }

  private flush(): void {
    if (!this.queue.length) return
    const now = Date.now()
    const toSend = this.queue.filter(op => now - op.queuedAt < MAX_AGE_MS)
    toSend.forEach(op => collaborationService.send(op.env))
    this.queue = []
    this.notifyChange()
  }

  private notifyChange(): void {
    this.changeHandlers.forEach(h => h(this.queue))
  }
}

export const syncManager = new SyncManager()
