import {useCallback, useEffect, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {adminApi} from '@/api/admin'
import {useConfirm, useToast} from '@/components/shared'
import {useAdminStore} from '@/stores/adminStore'
import {useAdminQuery} from './useAdminQuery'

// The legacy-data migration: its status (polled every 5 s only while a run
// is in progress, so an idle admin page costs nothing) and the confirmed
// start action.
export function useMigration() {
  const {t} = useTranslation('admin')
  const nonce = useAdminStore(s => s.nonce)
  const {confirm} = useConfirm()
  const toast = useToast()
  const toastRef = useRef(toast)
  useEffect(() => {
    toastRef.current = toast
  })

  const [poll, setPoll] = useState(0)
  const result = useAdminQuery(
    `migration#${nonce}#${poll}`,
    signal => adminApi.getMigrationStatus(signal),
    message => toastRef.current.error(t('migration.statusFailed'), {description: message}),
  )

  const running = result.data?.running ?? false
  useEffect(() => {
    if (!running) return
    const id = setInterval(() => setPoll(n => n + 1), 5000)
    return () => clearInterval(id)
  }, [running])

  const [isStarting, setIsStarting] = useState(false)
  const start = useCallback(async () => {
    const ok = await confirm({
      title: t('migration.confirmTitle'),
      message: t('migration.confirmBody'),
      confirmLabel: t('migration.confirmLabel'),
    })
    if (!ok) return
    setIsStarting(true)
    try {
      await adminApi.startMigration()
      setPoll(n => n + 1)
    } catch (e) {
      toastRef.current.error(t('migration.startFailed'), {description: e instanceof Error ? e.message : String(e)})
    } finally {
      setIsStarting(false)
    }
  }, [confirm, t])

  return {
    status: result.data,
    error: result.error,
    isLoading: result.isLoading,
    isRefetching: result.isRefetching,
    retry: result.retry,
    isStarting,
    start,
  }
}
