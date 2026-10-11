import {adminApi} from '@/api/admin'
import {useAdminStore} from '@/stores/adminStore'
import {useAdminQuery} from './useAdminQuery'

export function useUserStats() {
  const nonce = useAdminStore(s => s.nonce)
  const usersNonce = useAdminStore(s => s.usersNonce)
  return useAdminQuery(`stats#${nonce}#${usersNonce}`, signal => adminApi.userStats(signal))
}
