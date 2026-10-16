import {useEffect, useRef} from 'react'
import {useTranslation} from 'react-i18next'
import {adminApi} from '@/api/admin'
import {useToast} from '@/components/shared'
import {MAX_LIST_OFFSET, pastEnd, trailCursor} from '@/lib/pageTrail'
import {useAdminStore} from '@/stores/adminStore'
import {useAdminQuery} from './useAdminQuery'

export function useAuditLog() {
  const {t} = useTranslation('admin')
  const filter = useAdminStore(s => s.audit)
  const nonce = useAdminStore(s => s.nonce)
  const setAuditPage = useAdminStore(s => s.setAuditPage)
  const trail = useAdminStore(s => s.auditTrail)
  const noteAuditPage = useAdminStore(s => s.noteAuditPage)
  const noteAuditEnd = useAdminStore(s => s.noteAuditEnd)
  const toast = useToast()
  const toastRef = useRef(toast)
  useEffect(() => {
    toastRef.current = toast
  })

  const key = [filter.action, filter.userId, filter.page, filter.pageSize, nonce].join('\u0000')
  const trailKey = [filter.action, filter.userId, filter.pageSize].join('\u0000')
  const result = useAdminQuery(
    key,
    async signal => {
      // While the next page is in offset reach, a page asks for one event more than it shows, which tells whether
      // more follow. The last page in reach asks for its own and follows the X-Next-Cursor it answers (sent while
      // events follow), beside the offset an older server pages by: past the server's offset cap no other way leads.
      const cursor = trailCursor(useAdminStore.getState().auditTrail, trailKey, filter.page)
      const peek = !cursor && (filter.page + 1) * filter.pageSize <= MAX_LIST_OFFSET
      const res = await adminApi.listAuditEvents(
        {
          action: filter.action || undefined,
          userId: filter.userId || undefined,
          limit: peek ? filter.pageSize + 1 : filter.pageSize,
          offset: filter.page * filter.pageSize,
          cursor,
        },
        signal,
      )
      noteAuditPage(trailKey, filter.page, peek ? undefined : res.nextCursor, res.nextCursor !== undefined)
      const more = peek ? res.events.length > filter.pageSize : res.nextCursor !== undefined || undefined
      return {events: res.events.slice(0, filter.pageSize), more}
    },
    message => toastRef.current.error(t('audit.loadFailed'), {description: message}),
  )

  const {data} = result
  const settled = !result.isLoading && !result.isRefetching && result.error === null
  // A full page without a cursor ends the log on a server that sends them; one that never has (it predates them)
  // leaves it open until the page after it is found empty.
  const hasMore =
    data !== null &&
    (data.more ??
      (data.events.length === filter.pageSize && !trail.cursors && !pastEnd(trail, trailKey, filter.page + 1)))

  useEffect(() => {
    if (!settled || !data || data.events.length > 0 || filter.page === 0) return
    noteAuditEnd(trailKey, filter.page)
    setAuditPage(filter.page - 1)
  }, [settled, data, filter.page, trailKey, noteAuditEnd, setAuditPage])

  return {
    events: data ? data.events : null,
    hasMore,
    error: result.error,
    isLoading: result.isLoading,
    isRefetching: result.isRefetching,
    retry: result.retry,
  }
}
