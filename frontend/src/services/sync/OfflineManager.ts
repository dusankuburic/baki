import { collaborationService, type ConnectionStatus } from '@/services/collaboration/CollaborationService'
import { syncManager } from '@/services/sync/SyncManager'
import { indexedDBStorage } from '@/services/storage/IndexedDBStorage'

export type OnlineStatus = 'online' | 'offline'

type OnlineStatusHandler = (status: OnlineStatus) => void

const FLUSH_RETRY_INTERVAL_MS = 5_000

class OfflineManager {
  private onlineStatus: OnlineStatus = navigator.onLine ? 'online' : 'offline'
  private handlers = new Set<OnlineStatusHandler>()
  private retryTimer: ReturnType<typeof setInterval> | null = null
  private unsubscribeCollabStatus: (() => void) | null = null
  private started = false

  start(): void {
    if (this.started) return
    this.started = true

    window.addEventListener('online', this.handleBrowserOnline)
    window.addEventListener('offline', this.handleBrowserOffline)

    this.unsubscribeCollabStatus = collaborationService.onStatusChange(this.handleCollabStatus)

    if (this.onlineStatus === 'online') {
      this.startRetryLoop()
    }
  }

  stop(): void {
    this.started = false
    window.removeEventListener('online', this.handleBrowserOnline)
    window.removeEventListener('offline', this.handleBrowserOffline)
    this.unsubscribeCollabStatus?.()
    this.unsubscribeCollabStatus = null
    this.stopRetryLoop()
  }

  getStatus(): OnlineStatus {
    return this.onlineStatus
  }

  isOnline(): boolean {
    return this.onlineStatus === 'online'
  }

  onStatusChange(handler: OnlineStatusHandler): () => void {
    this.handlers.add(handler)
    return () => this.handlers.delete(handler)
  }

  /**
   * Persists an op to IndexedDB for later delivery when offline.
   */
  async persistOp(id: string, payload: unknown): Promise<void> {
    await indexedDBStorage.enqueuePendingOp({ id, payload, queuedAt: Date.now() })
  }

  /**
   * Flushes all persisted ops through the sync manager and clears them from
   * IndexedDB once sent.
   */
  async flushPersistedOps(): Promise<void> {
    if (collaborationService.getStatus() !== 'connected') return

    const ops = await indexedDBStorage.listPendingOps()
    for (const op of ops) {
      try {
        syncManager.enqueue(op.payload as Parameters<typeof syncManager.enqueue>[0])
        await indexedDBStorage.deletePendingOp(op.id)
      } catch {
        // Leave op in IndexedDB for the next flush attempt
      }
    }
  }

  private handleBrowserOnline = (): void => {
    this.setStatus('online')
    this.startRetryLoop()
    void this.flushPersistedOps()
  }

  private handleBrowserOffline = (): void => {
    this.setStatus('offline')
    this.stopRetryLoop()
  }

  private handleCollabStatus = (status: ConnectionStatus): void => {
    if (status === 'connected') {
      void this.flushPersistedOps()
    }
  }

  private startRetryLoop(): void {
    if (this.retryTimer !== null) return
    this.retryTimer = setInterval(() => {
      void this.flushPersistedOps()
    }, FLUSH_RETRY_INTERVAL_MS)
  }

  private stopRetryLoop(): void {
    if (this.retryTimer !== null) {
      clearInterval(this.retryTimer)
      this.retryTimer = null
    }
  }

  private setStatus(s: OnlineStatus): void {
    if (this.onlineStatus === s) return
    this.onlineStatus = s
    this.handlers.forEach(h => h(s))
  }
}

export const offlineManager = new OfflineManager()
