import {useTranslation} from 'react-i18next'
import {useEffect} from 'react'
import {syncManager} from '@/services/sync/SyncManager'
import {useToast} from '@/components/shared'

// SyncDropNotifier surfaces silent sync-queue drops as a toast. The queue holds
// cursor/presence updates for the collaboration socket — nothing else is ever
// queued offline — and it can discard them (TTL expiry > 60s offline, or
// overflow past 200 queued) with no signal. The toast names what was dropped
// so nobody reads "changes not synced" as lost edits.
//
// `warning` from useToast is a stable memoized callback, so the effect binds the
// syncManager subscription once for the component's lifetime (the [warning] dep
// never re-fires in practice).
export default function SyncDropNotifier() {
  const {t} = useTranslation()
  const {warning} = useToast()
  useEffect(() => {
    return syncManager.onDroppedOps((count, reason) => {
      warning(t('syncDrop.title', {count}), {
        description: reason === 'expired' ? t('syncDrop.expired') : t('syncDrop.overflow'),
      })
    })
  }, [warning, t])
  return null
}
