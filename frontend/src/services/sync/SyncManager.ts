import {collaborationService, type Envelope, type ConnectionStatus} from '@/services/collaboration/CollaborationService'

export interface PendingOp {
  id: string
  env: Omit<Envelope, 'flowId' | 'userId' | 'ts'>
  queuedAt: number
}

interface PersistedQueue {
  queue: PendingOp[]
  counter: number
}

type QueueChangeHandler = (queue: readonly PendingOp[]) => void

export type SyncDropReason = 'expired' | 'overflow'
type DropHandler = (count: number, reason: SyncDropReason) => void

const MAX_AGE_MS = 60_000
const STORAGE_PREFIX = 'baki-sync-queue-'
const MAX_QUEUE_SIZE = 200

class SyncManager {
  private queue: PendingOp[] = []
  private changeHandlers = new Set<QueueChangeHandler>()
  private dropHandlers = new Set<DropHandler>()
  private unsubscribeStatus: (() => void) | null = null
  private counter = 0
  private flowId: string | null = null

  start(flowId: string): void {
    if (this.unsubscribeStatus && this.flowId === flowId) return
    if (this.flowId !== flowId) {
      this.queue = []
      this.flowId = flowId
      this.loadFromStorage()
    }
    this.unsubscribeStatus?.()
    this.unsubscribeStatus = collaborationService.onStatusChange(this.handleStatusChange)
  }

  stop(): void {
    this.saveToStorage()
    this.unsubscribeStatus?.()
    this.unsubscribeStatus = null
    this.queue = []
    this.flowId = null
    this.notifyChange()
  }

  reset(): void {
    this.clearAllStorage()
    this.unsubscribeStatus?.()
    this.unsubscribeStatus = null
    this.queue = []
    this.counter = 0
    this.flowId = null
    this.notifyChange()
  }

  enqueue(env: Omit<Envelope, 'flowId' | 'userId' | 'ts'>): string {
    const id = `op-${++this.counter}-${Date.now()}`
    this.queue.push({id, env, queuedAt: Date.now()})
    if (this.queue.length > MAX_QUEUE_SIZE) {
      const overflow = this.queue.length - MAX_QUEUE_SIZE
      this.queue = this.queue.slice(-MAX_QUEUE_SIZE)
      this.notifyDropped(overflow, 'overflow')
    }
    this.saveToStorage()
    this.notifyChange()

    if (collaborationService.getStatus() === 'connected') {
      this.flush()
    }
    return id
  }

  getQueue(): readonly PendingOp[] {
    return this.queue
  }

  onQueueChange(handler: QueueChangeHandler): () => void {
    this.changeHandlers.add(handler)
    return () => this.changeHandlers.delete(handler)
  }

  onDroppedOps(handler: DropHandler): () => void {
    this.dropHandlers.add(handler)
    return () => this.dropHandlers.delete(handler)
  }

  private notifyDropped(count: number, reason: SyncDropReason): void {
    if (count > 0) this.dropHandlers.forEach(h => h(count, reason))
  }

  private handleStatusChange = (status: ConnectionStatus): void => {
    if (status === 'connected') this.flush()
  }

  private flush(): void {
    if (!this.queue.length) return
    const now = Date.now()
    let expired = 0
    const remaining: PendingOp[] = []
    for (const op of this.queue) {
      if (now - op.queuedAt >= MAX_AGE_MS) {
        expired++
        continue
      }
      if (!collaborationService.send(op.env)) {
        remaining.push(op)
      }
    }
    this.notifyDropped(expired, 'expired')
    this.queue = remaining
    if (remaining.length === 0) {
      this.clearStorage()
    } else {
      this.saveToStorage()
    }
    this.notifyChange()
  }

  private notifyChange(): void {
    this.changeHandlers.forEach(h => h(this.queue))
  }

  private storageKey(): string {
    return `${STORAGE_PREFIX}${this.flowId}`
  }

  private saveToStorage(): void {
    if (!this.flowId) return
    try {
      const data: PersistedQueue = {queue: this.queue, counter: this.counter}
      localStorage.setItem(this.storageKey(), JSON.stringify(data))
    } catch {
    }
  }

  private loadFromStorage(): void {
    if (!this.flowId) return
    try {
      const raw = localStorage.getItem(this.storageKey())
      if (!raw) return
      const data = JSON.parse(raw) as PersistedQueue
      const now = Date.now()
      const fresh = data.queue.filter(op => now - op.queuedAt < MAX_AGE_MS)
      this.queue = fresh
      this.counter = data.counter
      this.notifyDropped(data.queue.length - fresh.length, 'expired')
      this.notifyChange()
    } catch {
    }
  }

  private clearStorage(): void {
    if (!this.flowId) return
    try {
      localStorage.removeItem(this.storageKey())
    } catch {
    }
  }

  private clearAllStorage(): void {
    try {
      const keys: string[] = []
      for (let i = 0; i < localStorage.length; i++) {
        const k = localStorage.key(i)
        if (k && k.startsWith(STORAGE_PREFIX)) keys.push(k)
      }
      keys.forEach(k => localStorage.removeItem(k))
    } catch {
    }
  }
}

export const syncManager = new SyncManager()
