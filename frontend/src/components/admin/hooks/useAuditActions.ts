import {adminApi} from '@/api/admin'
import {useAdminStore} from '@/stores/adminStore'
import {useAdminQuery} from './useAdminQuery'

// The server's closed set of audit action ids, for the action filter.
export function useAuditActions() {
  const nonce = useAdminStore(s => s.nonce)
  return useAdminQuery(`actions#${nonce}`, signal => adminApi.listAuditActions(signal))
}
