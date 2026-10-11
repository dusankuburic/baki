import {create} from 'zustand'
import {syncManager, type PendingOp} from '@/services/sync/SyncManager'

interface SyncState {
  queue: readonly PendingOp[]
  pendingCount: number
}

export const useSyncStore = create<SyncState>(() => ({
  queue: [],
  pendingCount: 0,
}))

syncManager.onQueueChange(queue => {
  useSyncStore.setState({queue, pendingCount: queue.length})
})
