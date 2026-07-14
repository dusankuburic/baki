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

const MAX_AGE_MS = 60_000
const STORAGE_PREFIX = 'baki-sync-queue-'
const MAX_QUEUE_SIZE = 200

class SyncManager {
  private queue: PendingOp[] = []
  private changeHandlers = new Set<QueueChangeHandler>()
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
    this.unsubscribeStatus = collaborationService.onStatusChange(this.handleStatusChange)
  }

  stop(): void {
    this.saveToStorage()
    this.unsubscribeStatus?.()
    this.unsubscribeStatus = null
    this.queue = []
    // Reset flowId (AFTER saveToStorage, which keys off it) so a subsequent
    // start() with the SAME flow re-runs loadFromStorage() and resumes the
    // queue persisted above; without this, start(sameFlowId) skipped the load
    // and the persisted ops were orphaned.
    this.flowId = null
    this.notifyChange()
  }

  // reset discards the queue AND every persisted per-flow copy — for session
  // teardown (logout). Unlike stop(), which persists the queue so an in-progress
  // flow can resume after a reconnect, no queue must survive into the next user's
  // session on a shared device. It clears ALL baki-sync-queue-* keys (not just
  // the active flow's): switching flows orphans the previous flow's persisted
  // queue (start() clears memory but not storage), so clearing only the current
  // flowId would leave earlier flows' ops behind to leak.
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
    // Always queue first so the op survives a send failure (socket
    // closing between the status check and the actual ws.send).
    this.queue.push({id, env, queuedAt: Date.now()})
    if (this.queue.length > MAX_QUEUE_SIZE) {
      this.queue = this.queue.slice(-MAX_QUEUE_SIZE)
    }
    this.saveToStorage()
    this.notifyChange()

    // Attempt immediate delivery if connected. flush() only removes
    // ops that were actually delivered; undelivered ops stay queued.
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

  private handleStatusChange = (status: ConnectionStatus): void => {
    if (status === 'connected') this.flush()
  }

  private flush(): void {
    if (!this.queue.length) return
    const now = Date.now()
    const remaining: PendingOp[] = []
    for (const op of this.queue) {
      if (now - op.queuedAt >= MAX_AGE_MS) continue // expired
      if (collaborationService.send(op.env)) {
        // delivered — drop from queue
      } else {
        remaining.push(op) // socket not open — keep for retry
      }
    }
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
      // Storage full or unavailable (private mode) — ops stay in memory only
    }
  }

  private loadFromStorage(): void {
    if (!this.flowId) return
    try {
      const raw = localStorage.getItem(this.storageKey())
      if (!raw) return
      const data = JSON.parse(raw) as PersistedQueue
      const now = Date.now()
      this.queue = data.queue.filter(op => now - op.queuedAt < MAX_AGE_MS)
      this.counter = data.counter
      this.notifyChange()
    } catch {
      // Corrupt data — start fresh
    }
  }

  private clearStorage(): void {
    if (!this.flowId) return
    try {
      localStorage.removeItem(this.storageKey())
    } catch {
      // ignore
    }
  }

  // clearAllStorage removes every persisted per-flow queue (all baki-sync-queue-*
  // keys), used by reset() on logout. Keys are collected before removal because
  // removeItem during a length/key(i) walk shifts indices.
  private clearAllStorage(): void {
    try {
      const keys: string[] = []
      for (let i = 0; i < localStorage.length; i++) {
        const k = localStorage.key(i)
        if (k && k.startsWith(STORAGE_PREFIX)) keys.push(k)
      }
      keys.forEach(k => localStorage.removeItem(k))
    } catch {
      // storage unavailable — nothing to clear
    }
  }
}

export const syncManager = new SyncManager()
