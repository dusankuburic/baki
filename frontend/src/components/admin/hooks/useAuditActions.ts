import {adminApi} from '@/api/admin'
import {useAdminStore} from '@/stores/adminStore'
import {useAdminQuery} from './useAdminQuery'

export function useAuditActions() {
  const nonce = useAdminStore(s => s.nonce)
  return useAdminQuery(`actions#${nonce}`, signal => adminApi.listAuditActions(signal))
}
