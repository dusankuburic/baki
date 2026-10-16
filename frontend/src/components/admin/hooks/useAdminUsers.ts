import {getErrorMessage} from '@/lib/getErrorMessage'
import {useCallback, useEffect, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {adminApi, type AdminUser, type UserRole} from '@/api/admin'
import {ApiError} from '@/api/client'
import {useConfirm, useToast} from '@/components/shared'
import {trailCursor} from '@/lib/pageTrail'
import {useAdminStore} from '@/stores/adminStore'
import {useAuthStore} from '@/stores/authStore'
import {roleLabel} from '../roles'
import {useAdminQuery} from './useAdminQuery'

export function useAdminUsers() {
  const {t} = useTranslation('admin')
  const list = useAdminStore(s => s.users)
  const nonce = useAdminStore(s => s.nonce)
  const usersNonce = useAdminStore(s => s.usersNonce)
  const setUsersPage = useAdminStore(s => s.setUsersPage)
  const touchUsers = useAdminStore(s => s.touchUsers)
  const noteUsersPage = useAdminStore(s => s.noteUsersPage)
  const currentUserId = useAuthStore(s => s.user?.id)
  const {confirm} = useConfirm()
  const toast = useToast()
  const toastRef = useRef(toast)
  useEffect(() => {
    toastRef.current = toast
  })

  const key = [list.q, list.role, list.page, list.pageSize, nonce, usersNonce].join('\u0000')
  const query = useMemo(
    () => ({
      q: list.q || undefined,
      role: list.role || undefined,
      limit: list.pageSize,
      offset: list.page * list.pageSize,
    }),
    [list.q, list.role, list.page, list.pageSize],
  )
  // A page past the first goes by the cursor its page before answered (beside the offset an older server pages by):
  // past the server's offset cap no other way leads.
  const trailKey = [list.q, list.role, list.pageSize].join('\u0000')
  const result = useAdminQuery(
    key,
    async signal => {
      const cursor = trailCursor(useAdminStore.getState().usersTrail, trailKey, list.page)
      const res = await adminApi.listUsers({...query, cursor}, signal)
      noteUsersPage(trailKey, list.page, res.nextCursor)
      return res
    },
    message => toastRef.current.error(t('users.loadFailed'), {description: message}),
  )

  const {data} = result
  // This page's own answer, not the page before's still shown while it loads.
  const settled = !result.isLoading && !result.isRefetching && result.error === null
  useEffect(() => {
    if (settled && data && data.items.length === 0 && data.total > 0 && list.page > 0) {
      // Back, never on: a page before this one is reachable (by offset, or by the cursor that led here).
      setUsersPage(Math.max(0, Math.min(list.page - 1, Math.ceil(data.total / list.pageSize) - 1)))
    }
  }, [settled, data, list.page, list.pageSize, setUsersPage])

  const [busyUserId, setBusyUserId] = useState<string | null>(null)
  const changeRole = useCallback(
    async (user: AdminUser, role: UserRole): Promise<boolean> => {
      if (role === user.role) return false
      const self = user.id === currentUserId
      const ok =
        self && role !== 'admin'
          ? await confirm({
              title: t('users.selfDemoteTitle'),
              message: t('users.selfDemoteBody'),
              confirmLabel: t('users.selfDemoteLabel'),
              danger: true,
            })
          : await confirm({
              title: t('users.confirmTitle'),
              message: t('users.confirmBody', {
                user: user.email,
                from: roleLabel(t, user.role),
                to: roleLabel(t, role),
              }),
              confirmLabel: t('users.confirmLabel'),
            })
      if (!ok) return false
      setBusyUserId(user.id)
      try {
        await adminApi.setUserRole(user.id, role)
        toastRef.current.success(t('users.roleChanged'), {
          description: t('users.roleChangedBody', {user: user.email, role: roleLabel(t, role)}),
        })
        touchUsers()
        return true
      } catch (e) {
        toastRef.current.error(t('users.roleFailed'), {description: getErrorMessage(e)})
        if (e instanceof ApiError && e.status === 404) touchUsers()
        return false
      } finally {
        setBusyUserId(null)
      }
    },
    [confirm, currentUserId, t, touchUsers],
  )

  return {...result, changeRole, busyUserId}
}
