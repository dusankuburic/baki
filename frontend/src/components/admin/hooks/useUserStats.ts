import {adminApi} from '@/api/admin'
import {useAdminStore} from '@/stores/adminStore'
import {useAdminQuery} from './useAdminQuery'

// The overview's counts: refetched by the header Refresh and after a role
// change (the buckets move), never by paging or filtering the list.
export function useUserStats() {
  const nonce = useAdminStore(s => s.nonce)
  const usersNonce = useAdminStore(s => s.usersNonce)
  return useAdminQuery(`stats#${nonce}#${usersNonce}`, signal => adminApi.userStats(signal))
}
