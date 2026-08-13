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

/** SyncDropReason explains why queued ops were discarded. */
export type SyncDropReason = 'expired' | 'overflow'
type DropHandler = (count: number, reason: SyncDropReason) => void

const MAX_AGE_MS = 60_000
const STORAGE_PREFIX = 'baki-sync-queue-'
const MAX_QUEUE_SIZE = 200

// The collaboration sync queue is a STATE-sync stream (presence, block/cursor
// positions): a newer op for the same element supersedes the older one. So
// under overflow we drop the OLDEST ops (keep current state) and past MAX_AGE_MS
// we discard stale positions — both correct for superseding state.
// What was a bug was doing this SILENTLY: the user saw "N queued" then it became
// 0 with no indication the stale ops were discarded vs delivered. The drop
// handlers below let the UI toast the user when ops are dropped (a true
// end-to-end delivery guarantee would need a deferred backend ack).
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
    // Drop any prior status subscription before re-subscribing. Switching flows
    // routes through start() WITHOUT an intervening stop() (presenceStore.connectToFlow
    // calls teardown()+start(), never syncManager.stop()), so without this the
    // previous subscription's unsubscribe handle is overwritten and lost. It's
    // currently harmless only because collaborationService dedups the (stable)
    // handler ref in a Set; unsubscribing here makes this store's cleanup
    // self-contained rather than depending on that external detail.
    this.unsubscribeStatus?.()
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
      const overflow = this.queue.length - MAX_QUEUE_SIZE
      this.queue = this.queue.slice(-MAX_QUEUE_SIZE)
      this.notifyDropped(overflow, 'overflow')
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

  /** Subscribe to drop events (ops discarded on TTL expiry or queue overflow). */
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
        expired++ // stale state op, superseded by a newer one
        continue
      }
      if (collaborationService.send(op.env)) {
        // delivered — drop from queue
      } else {
        remaining.push(op) // socket not open — keep for retry
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
      const fresh = data.queue.filter(op => now - op.queuedAt < MAX_AGE_MS)
      this.queue = fresh
      this.counter = data.counter
      this.notifyDropped(data.queue.length - fresh.length, 'expired')
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
