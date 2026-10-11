import {useEffect, useMemo, useRef} from 'react'
import {useTranslation} from 'react-i18next'
import {adminApi} from '@/api/admin'
import {useToast} from '@/components/shared'
import {useAdminStore} from '@/stores/adminStore'
import {useAdminQuery} from './useAdminQuery'

export function useAuditLog() {
  const {t} = useTranslation('admin')
  const filter = useAdminStore(s => s.audit)
  const nonce = useAdminStore(s => s.nonce)
  const setAuditPage = useAdminStore(s => s.setAuditPage)
  const toast = useToast()
  const toastRef = useRef(toast)
  useEffect(() => {
    toastRef.current = toast
  })

  const key = [filter.action, filter.userId, filter.page, filter.pageSize, nonce].join('\u0000')
  const result = useAdminQuery(
    key,
    signal =>
      adminApi.listAuditEvents(
        {
          action: filter.action || undefined,
          userId: filter.userId || undefined,
          limit: filter.pageSize + 1,
          offset: filter.page * filter.pageSize,
        },
        signal,
      ),
    message => toastRef.current.error(t('audit.loadFailed'), {description: message}),
  )

  const {data} = result
  const events = useMemo(() => (data ? data.slice(0, filter.pageSize) : null), [data, filter.pageSize])
  const hasMore = data !== null && data.length > filter.pageSize

  useEffect(() => {
    if (data && data.length === 0 && filter.page > 0) setAuditPage(filter.page - 1)
  }, [data, filter.page, setAuditPage])

  return {
    events,
    hasMore,
    error: result.error,
    isLoading: result.isLoading,
    isRefetching: result.isRefetching,
    retry: result.retry,
  }
}
