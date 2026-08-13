import {useEffect} from 'react'
import {syncManager} from '@/services/sync/SyncManager'
import {useToast} from '@/components/shared'

// SyncDropNotifier surfaces silent sync-queue drops as a toast. Without it, the
// offline sync queue can discard ops (TTL expiry > 60s offline, or overflow
// past 200 queued) with no signal: the OfflineIndicator count silently goes to
// 0 and the user believes stale changes were delivered when they were discarded.
// This is the client-side half of the fix; a true end-to-end delivery guarantee
// would additionally need a backend send-ack.
//
// `warning` from useToast is a stable memoized callback, so the effect binds the
// syncManager subscription once for the component's lifetime (the [warning] dep
// never re-fires in practice).
export default function SyncDropNotifier() {
  const {warning} = useToast()
  useEffect(() => {
    return syncManager.onDroppedOps((count, reason) => {
      warning(`${count} change(s) not synced`, {
        description:
          reason === 'expired'
            ? 'You were offline past the 60s sync window. Recent positions are kept; stale ones were discarded.'
            : 'The pending-change buffer was full; oldest entries were discarded to keep the latest state.',
      })
    })
  }, [warning])
  return null
}
