import {useState, useEffect, useCallback, useRef} from 'react'
import {adminApi, type MigrationStatus, type AuditEvent} from '@/api/admin'
import {type AuthUser} from '@/api/auth'
import {useConfirm} from '@/components/shared'
import {logger} from '@/lib/logger'

const AUDIT_PAGE_SIZE = 50

export function useAdminData(isAdmin: boolean) {
  const {confirm} = useConfirm()
  const [migrationStatus, setMigrationStatus] = useState<MigrationStatus | null>(null)
  const [users, setUsers] = useState<AuthUser[]>([])
  const [auditEvents, setAuditEvents] = useState<AuditEvent[]>([])
  const [auditAction, setAuditAction] = useState('')
  const [auditUserId, setAuditUserId] = useState('')
  const [auditOffset, setAuditOffset] = useState(0)
  const [auditActions, setAuditActions] = useState<string[]>([])
  const [isLoading, setIsLoading] = useState(false)
  const [isStarting, setIsStarting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const statusRef = useRef<MigrationStatus | null>(null)
  const fetchStatus = useCallback(async () => {
    try {
      const data = await adminApi.getMigrationStatus()
      statusRef.current = data
      setMigrationStatus(data)
    } catch (err) {
      logger.warn('Failed to fetch migration status', err)
    }
  }, [])

  const fetchUsers = useCallback(async () => {
    try {
      setUsers(await adminApi.listUsers())
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch users')
    }
  }, [])

  // AUDIT_PAGE_SIZE rows per page; the backend honours offset/limit/userId
  // but the hook used to hardcode {limit: 100} with no way past row 100.
  const auditActionRef = useRef('')
  const auditUserRef = useRef('')
  const auditOffsetRef = useRef(0)
  const fetchAudit = useCallback(async (action?: string, userId?: string, offset?: number) => {
    const a = action ?? auditActionRef.current
    const u = userId ?? auditUserRef.current
    const o = offset ?? auditOffsetRef.current
    try {
      setAuditEvents(await adminApi.listAuditEvents({action: a || undefined, userId: u || undefined, limit: AUDIT_PAGE_SIZE, offset: o}))
    } catch {
      // audit log is optional
    }
  }, [])

  const fetchAuditActions = useCallback(async () => {
    try {
      setAuditActions(await adminApi.listAuditActions())
    } catch {
      // filter falls back to a free-text match on the action column
    }
  }, [])

  const fetchAll = useCallback(async () => {
    setIsLoading(true)
    await Promise.all([fetchStatus(), fetchUsers(), fetchAudit(), fetchAuditActions()])
    setIsLoading(false)
  }, [fetchStatus, fetchUsers, fetchAudit, fetchAuditActions])

  useEffect(() => {
    if (!isAdmin) return
    let cancelled = false
    const run = async () => {
      setIsLoading(true)
      await Promise.all([fetchStatus(), fetchUsers(), fetchAudit(), fetchAuditActions()])
      if (!cancelled) setIsLoading(false)
    }
    void run()
    // Only poll while a migration is actively running — avoids unnecessary
    // network traffic for the admin panel's lifetime when idle.
    const interval = setInterval(() => {
      if (!cancelled && statusRef.current?.running) void fetchStatus()
    }, 5000)
    return () => {
      cancelled = true
      clearInterval(interval)
    }
  }, [isAdmin, fetchStatus, fetchUsers, fetchAudit, fetchAuditActions])

  const startMigration = useCallback(async () => {
    const ok = await confirm({
      title: 'Start migration',
      message:
        'Copy flows, settings and chat conversations from the legacy data directory into the database? Already migrated flows are skipped.',
      confirmLabel: 'Start migration',
    })
    if (!ok) return
    setError(null)
    setIsStarting(true)
    try {
      await adminApi.startMigration()
      await fetchStatus()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to start migration')
    } finally {
      setIsStarting(false)
    }
  }, [fetchStatus, confirm])

  const changeRole = useCallback(
    async (userId: string, newRole: string, currentUserId?: string) => {
      if (userId === currentUserId && newRole !== 'admin') {
        const ok = await confirm({
          title: 'Remove your admin access',
          message: 'You are about to remove your own administrator privileges. Are you sure?',
          danger: true,
          confirmLabel: 'Remove access',
        })
        if (!ok) return
      }
      try {
        await adminApi.setUserRole(userId, newRole)
        await fetchUsers()
      } catch (err) {
        setError('Failed to change role: ' + (err instanceof Error ? err.message : 'Unknown error'))
      }
    },
    [fetchUsers, confirm],
  )

  // Any filter change restarts paging from the first page.
  const filterAudit = useCallback(
    (action: string, userId?: string) => {
      auditActionRef.current = action
      if (userId !== undefined) auditUserRef.current = userId
      auditOffsetRef.current = 0
      setAuditAction(action)
      if (userId !== undefined) setAuditUserId(userId)
      setAuditOffset(0)
      void fetchAudit(action, userId, 0)
    },
    [fetchAudit],
  )

  const pageAudit = useCallback(
    (direction: 'prev' | 'next') => {
      const next = Math.max(0, auditOffsetRef.current + (direction === 'next' ? AUDIT_PAGE_SIZE : -AUDIT_PAGE_SIZE))
      auditOffsetRef.current = next
      setAuditOffset(next)
      void fetchAudit(undefined, undefined, next)
    },
    [fetchAudit],
  )

  return {
    migrationStatus,
    users,
    auditEvents,
    auditAction,
    auditUserId,
    auditOffset,
    auditActions,
    auditPageSize: AUDIT_PAGE_SIZE,
    isLoading,
    isStarting,
    error,
    fetchAll,
    startMigration,
    changeRole,
    filterAudit,
    pageAudit,
  }
}
