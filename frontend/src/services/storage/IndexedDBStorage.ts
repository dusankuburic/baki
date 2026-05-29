const DB_NAME = 'pad-analyzer'
const DB_VERSION = 1

const STORES = {
  flows: 'flows',
  conversations: 'conversations',
  pendingOps: 'pending-ops',
} as const

export interface StoredFlow {
  id: string
  name: string
  content: unknown
  updatedAt: number
}

export interface StoredConversation {
  id: string      // `${flowId}::${provider}`
  flowId: string
  provider: string
  messages: unknown[]
  updatedAt: number
}

export interface StoredPendingOp {
  id: string
  payload: unknown
  queuedAt: number
}

class IndexedDBStorage {
  private db: IDBDatabase | null = null

  private async open(): Promise<IDBDatabase> {
    if (this.db) return this.db

    return new Promise((resolve, reject) => {
      const req = indexedDB.open(DB_NAME, DB_VERSION)

      req.onupgradeneeded = () => {
        const db = req.result
        if (!db.objectStoreNames.contains(STORES.flows)) {
          db.createObjectStore(STORES.flows, { keyPath: 'id' })
        }
        if (!db.objectStoreNames.contains(STORES.conversations)) {
          db.createObjectStore(STORES.conversations, { keyPath: 'id' })
        }
        if (!db.objectStoreNames.contains(STORES.pendingOps)) {
          db.createObjectStore(STORES.pendingOps, { keyPath: 'id' })
        }
      }

      req.onsuccess = () => {
        this.db = req.result
        resolve(req.result)
      }
      req.onerror = () => reject(req.error)
    })
  }

  private tx(
    storeName: string,
    mode: IDBTransactionMode,
    fn: (store: IDBObjectStore) => IDBRequest,
  ): Promise<unknown> {
    return this.open().then(
      db =>
        new Promise((resolve, reject) => {
          const tx = db.transaction(storeName, mode)
          const req = fn(tx.objectStore(storeName))
          req.onsuccess = () => resolve(req.result)
          req.onerror = () => reject(req.error)
        }),
    )
  }

  // ---- flows ----

  async saveFlow(flow: StoredFlow): Promise<void> {
    await this.tx(STORES.flows, 'readwrite', s => s.put(flow))
  }

  async loadFlow(id: string): Promise<StoredFlow | undefined> {
    return this.tx(STORES.flows, 'readonly', s => s.get(id)) as Promise<StoredFlow | undefined>
  }

  async listFlows(): Promise<StoredFlow[]> {
    return this.open().then(
      db =>
        new Promise((resolve, reject) => {
          const tx = db.transaction(STORES.flows, 'readonly')
          const req = tx.objectStore(STORES.flows).getAll()
          req.onsuccess = () => resolve(req.result as StoredFlow[])
          req.onerror = () => reject(req.error)
        }),
    )
  }

  async deleteFlow(id: string): Promise<void> {
    await this.tx(STORES.flows, 'readwrite', s => s.delete(id))
  }

  // ---- conversations ----

  async saveConversation(conv: StoredConversation): Promise<void> {
    await this.tx(STORES.conversations, 'readwrite', s => s.put(conv))
  }

  async loadConversation(flowId: string, provider: string): Promise<StoredConversation | undefined> {
    const id = `${flowId}::${provider}`
    return this.tx(
      STORES.conversations,
      'readonly',
      s => s.get(id),
    ) as Promise<StoredConversation | undefined>
  }

  async deleteConversation(flowId: string, provider: string): Promise<void> {
    await this.tx(STORES.conversations, 'readwrite', s => s.delete(`${flowId}::${provider}`))
  }

  // ---- pending ops ----

  async enqueuePendingOp(op: StoredPendingOp): Promise<void> {
    await this.tx(STORES.pendingOps, 'readwrite', s => s.put(op))
  }

  async listPendingOps(): Promise<StoredPendingOp[]> {
    return this.open().then(
      db =>
        new Promise((resolve, reject) => {
          const tx = db.transaction(STORES.pendingOps, 'readonly')
          const req = tx.objectStore(STORES.pendingOps).getAll()
          req.onsuccess = () => resolve(req.result as StoredPendingOp[])
          req.onerror = () => reject(req.error)
        }),
    )
  }

  async deletePendingOp(id: string): Promise<void> {
    await this.tx(STORES.pendingOps, 'readwrite', s => s.delete(id))
  }

  async clearPendingOps(): Promise<void> {
    await this.open().then(
      db =>
        new Promise<void>((resolve, reject) => {
          const tx = db.transaction(STORES.pendingOps, 'readwrite')
          const req = tx.objectStore(STORES.pendingOps).clear()
          req.onsuccess = () => resolve()
          req.onerror = () => reject(req.error)
        }),
    )
  }
}

export const indexedDBStorage = new IndexedDBStorage()
