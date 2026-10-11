import {useTranslation} from 'react-i18next'
import {useEffect} from 'react'
import {syncManager} from '@/services/sync/SyncManager'
import {useToast} from '@/components/shared'

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
