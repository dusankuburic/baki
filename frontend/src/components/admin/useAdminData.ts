import {useState, useEffect, useCallback, useRef} from 'react'
import {adminApi, type MigrationStatus, type AuditEvent} from '@/api/admin'
import {type AuthUser} from '@/api/auth'
import {useConfirm} from '@/components/shared'
import {logger} from '@/lib/logger'

export function useAdminData(isAdmin: boolean) {
  const {confirm} = useConfirm()
  const [migrationStatus, setMigrationStatus] = useState<MigrationStatus | null>(null)
  const [users, setUsers] = useState<AuthUser[]>([])
  const [auditEvents, setAuditEvents] = useState<AuditEvent[]>([])
  const [auditAction, setAuditAction] = useState('')
  const [isLoading, setIsLoading] = useState(false)
  const [isStarting, setIsStarting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const fetchStatus = useCallback(async () => {
    try {
      setMigrationStatus(await adminApi.getMigrationStatus())
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

  const auditActionRef = useRef('')
  const fetchAudit = useCallback(async (action?: string) => {
    const a = action ?? auditActionRef.current
    try {
      setAuditEvents(await adminApi.listAuditEvents({action: a || undefined, limit: 100}))
    } catch {
      // audit log is optional
    }
  }, [])

  const fetchAll = useCallback(async () => {
    setIsLoading(true)
    await Promise.all([fetchStatus(), fetchUsers(), fetchAudit()])
    setIsLoading(false)
  }, [fetchStatus, fetchUsers, fetchAudit])

  useEffect(() => {
    if (!isAdmin) return
    let cancelled = false
    const run = async () => {
      setIsLoading(true)
      await Promise.all([fetchStatus(), fetchUsers(), fetchAudit()])
      if (!cancelled) setIsLoading(false)
    }
    run()
    const interval = setInterval(() => { if (!cancelled) fetchStatus() }, 5000)
    return () => { cancelled = true; clearInterval(interval) }
  }, [isAdmin, fetchStatus, fetchUsers, fetchAudit])

  const startMigration = useCallback(async () => {
    const ok = await confirm({
      title: 'Start migration',
      message: 'Start data migration from filesystem to database? This will skip already migrated flows.',
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

  const changeRole = useCallback(async (userId: string, newRole: string, currentUserId?: string) => {
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
  }, [fetchUsers, confirm])

  const filterAudit = useCallback((action: string) => {
    auditActionRef.current = action
    setAuditAction(action)
    fetchAudit(action)
  }, [fetchAudit])

  return {
    migrationStatus, users, auditEvents, auditAction,
    isLoading, isStarting, error,
    fetchAll, startMigration, changeRole, filterAudit,
  }
}
