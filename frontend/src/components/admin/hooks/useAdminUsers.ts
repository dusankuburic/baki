import {getErrorMessage} from '@/lib/getErrorMessage'
import {useCallback, useEffect, useMemo, useRef, useState} from 'react'
import {useTranslation} from 'react-i18next'
import {adminApi, type AdminUser, type UserRole} from '@/api/admin'
import {ApiError} from '@/api/client'
import {useConfirm, useToast} from '@/components/shared'
import {useAdminStore} from '@/stores/adminStore'
import {useAuthStore} from '@/stores/authStore'
import {roleLabel} from '../roles'
import {useAdminQuery} from './useAdminQuery'

// The user list for the store's filters and page, and the one role-change
// action: a single confirmation (a danger dialog when it removes the caller's
// own admin access), the request, a toast either way, and a refetch of the
// same page plus the stats so the row and the tiles agree.
export function useAdminUsers() {
  const {t} = useTranslation('admin')
  const list = useAdminStore(s => s.users)
  const nonce = useAdminStore(s => s.nonce)
  const usersNonce = useAdminStore(s => s.usersNonce)
  const setUsersPage = useAdminStore(s => s.setUsersPage)
  const touchUsers = useAdminStore(s => s.touchUsers)
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
  const result = useAdminQuery(
    key,
    signal => adminApi.listUsers(query, signal),
    message => toastRef.current.error(t('users.loadFailed'), {description: message}),
  )

  // A filter that shrinks the set can leave the page past the end: step back
  // to the last page that has rows.
  const {data} = result
  useEffect(() => {
    if (data && data.items.length === 0 && data.total > 0 && list.page > 0) {
      setUsersPage(Math.max(0, Math.ceil(data.total / list.pageSize) - 1))
    }
  }, [data, list.page, list.pageSize, setUsersPage])

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
        // The account is gone: drop it from the page.
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
