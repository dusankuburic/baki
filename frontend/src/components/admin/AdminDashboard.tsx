import React, { useState, useEffect } from 'react'
import { Shield, Users, RefreshCw, CheckCircle2, XCircle, Loader, ScrollText, Filter } from 'lucide-react'
import clsx from 'clsx'
import { adminApi, type MigrationStatus, type AuditEvent } from '@/api/admin'
import { type AuthUser } from '@/api/auth'
import { useAuthStore } from '@/stores/authStore'
import Button from '@/components/shared/Button'

function roleBadgeClass(role: string) {
  switch (role) {
    case 'admin':   return 'bg-block-subflow/10 text-block-subflow'
    case 'member':  return 'bg-block-action/10 text-block-action'
    case 'viewer':  return 'bg-block-condition/10 text-block-condition'
    default:        return 'bg-surface-4 text-text-tertiary'
  }
}

function migrationStatusClass(status?: string) {
  switch (status) {
    case 'running':   return 'bg-semantic-info/10 text-semantic-info'
    case 'completed': return 'bg-semantic-success/10 text-semantic-success'
    default:          return 'bg-surface-4 text-text-tertiary'
  }
}

export const AdminDashboard: React.FC = () => {
  const { user: currentUser } = useAuthStore()
  const [migrationStatus, setMigrationStatus] = useState<MigrationStatus | null>(null)
  const [users, setUsers] = useState<AuthUser[]>([])
  const [auditEvents, setAuditEvents] = useState<AuditEvent[]>([])
  const [auditAction, setAuditAction] = useState('')
  const [isLoading, setIsLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const fetchStatus = async () => {
    try {
      const status = await adminApi.getMigrationStatus()
      setMigrationStatus(status)
    } catch (err) {
      console.error('Failed to fetch migration status', err)
    }
  }

  const fetchUsers = async () => {
    try {
      const u = await adminApi.listUsers()
      setUsers(u)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch users')
    }
  }

  const fetchAudit = async (action = auditAction) => {
    try {
      const events = await adminApi.listAuditEvents({ action: action || undefined, limit: 100 })
      setAuditEvents(events)
    } catch {
      // audit log is optional — don't surface as error
    }
  }

  const fetchData = async () => {
    setIsLoading(true)
    await Promise.all([fetchStatus(), fetchUsers(), fetchAudit()])
    setIsLoading(false)
  }

  useEffect(() => {
    if (currentUser?.role === 'admin') {
      fetchData()
      const interval = setInterval(fetchStatus, 5000)
      return () => clearInterval(interval)
    }
  }, [currentUser])

  const handleStartMigration = async () => {
    if (!confirm('Start data migration from filesystem to database? This will skip already migrated flows.')) return
    setError(null)
    try {
      await adminApi.startMigration()
      await fetchStatus()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to start migration')
    }
  }

  const handleRoleChange = async (userId: string, newRole: string) => {
    if (userId === currentUser?.id && newRole !== 'admin') {
      if (!confirm('You are about to remove your own administrator privileges. Are you sure?')) return
    }
    try {
      await adminApi.setUserRole(userId, newRole)
      await fetchUsers()
    } catch (err) {
      setError('Failed to change role: ' + (err instanceof Error ? err.message : 'Unknown error'))
    }
  }

  if (currentUser?.role !== 'admin') {
    return (
      <div className="p-6 flex items-center gap-2 text-semantic-error text-sm">
        <Shield size={16} />
        <span>Access Denied: Administrator role required.</span>
      </div>
    )
  }

  const adminCount  = users.filter(u => u.role === 'admin').length
  const memberCount = users.filter(u => u.role === 'member').length
  const otherCount  = users.filter(u => u.role !== 'admin' && u.role !== 'member').length

  return (
    <div className="p-6 md:p-8 space-y-6 max-w-5xl mx-auto">

      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-text-primary">Admin Dashboard</h1>
          <p className="text-sm text-text-tertiary mt-0.5">Manage users and system settings</p>
        </div>
        <Button
          variant="ghost"
          size="sm"
          icon={isLoading ? Loader : RefreshCw}
          onClick={fetchData}
          disabled={isLoading}
          className={clsx(isLoading && '[&_svg]:animate-spin')}
        >
          {isLoading ? 'Refreshing…' : 'Refresh'}
        </Button>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-3">
        <StatCard icon={Users}  label="Total Users" value={users.length} color="text-text-secondary" />
        <StatCard icon={Shield} label="Admins"       value={adminCount}  color="text-block-subflow" />
        <StatCard icon={Users}  label="Members"      value={memberCount} color="text-block-action" />
        <StatCard icon={Users}  label="Other"        value={otherCount}  color="text-text-tertiary" />
      </div>

      {/* User Management */}
      <section className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
        <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
          <Users size={14} className="text-text-tertiary" />
          <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">User Management</h2>
        </div>
        <div className="overflow-x-auto">
          <table className="min-w-full divide-y divide-border-subtle">
            <thead className="bg-surface-3">
              <tr>
                {['Email', 'ID', 'Role', 'Change Role'].map(h => (
                  <th key={h} className="px-5 py-2.5 text-left text-xs font-medium text-text-tertiary uppercase tracking-wider">
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-border-subtle">
              {users.map(u => (
                <tr key={u.id} className="bg-surface-2 hover:bg-surface-3 transition-colors duration-fast">
                  <td className="px-5 py-3 text-sm font-medium text-text-primary max-w-[200px]"><span className="block truncate">{u.email}</span></td>
                  <td className="px-5 py-3 text-xs text-text-tertiary font-mono max-w-[120px] truncate">{u.id}</td>
                  <td className="px-5 py-3 whitespace-nowrap">
                    <span className={clsx(
                      'px-2 py-0.5 rounded-md text-xs font-semibold uppercase',
                      roleBadgeClass(u.role)
                    )}>
                      {u.role}
                    </span>
                  </td>
                  <td className="px-5 py-3 whitespace-nowrap">
                    <select
                      value={u.role}
                      onChange={e => handleRoleChange(u.id, e.target.value)}
                      className="bg-surface-3 border border-border-default rounded-md text-text-primary text-xs px-2 py-1 focus:outline-none focus:border-brand-500 transition-colors"
                    >
                      <option value="admin">Admin</option>
                      <option value="member">Member</option>
                      <option value="viewer">Viewer</option>
                      <option value="guest">Guest</option>
                    </select>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      {/* Data Migration */}
      <section className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
        <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
          <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Data Migration</h2>
          <span className="text-xs text-text-tertiary">Local → Cloud</span>
          {migrationStatus?.status && (
            <span className={clsx(
              'ml-auto px-2.5 py-0.5 rounded-full text-xs font-semibold uppercase',
              migrationStatusClass(migrationStatus.status)
            )}>
              {migrationStatus.status}
            </span>
          )}
        </div>

        <div className="p-5 space-y-4">
          {migrationStatus?.result && (
            <div className="grid grid-cols-2 md:grid-cols-4 gap-3 bg-surface-3 border border-border-subtle rounded-lg p-3">
              <MigStat label="Migrated" value={migrationStatus.result.FlowsMigrated} />
              <MigStat label="Skipped"  value={migrationStatus.result.FlowsSkipped} />
              <MigStat label="Failed"   value={migrationStatus.result.FlowsFailed} />
              <div className="flex flex-col gap-1">
                <span className="text-xs text-text-tertiary">Settings</span>
                <span className="flex items-center gap-1">
                  {migrationStatus.result.SettingsMoved
                    ? <CheckCircle2 size={14} className="text-semantic-success" />
                    : <XCircle size={14} className="text-semantic-error" />}
                  <span className="text-sm font-semibold text-text-primary">
                    {migrationStatus.result.SettingsMoved ? 'Moved' : 'Not moved'}
                  </span>
                </span>
              </div>
              <div className="col-span-full pt-2 border-t border-border-subtle flex items-center gap-2">
                <span className="text-xs text-text-tertiary">Duration</span>
                <span className="text-sm font-semibold text-text-primary">
                  {(migrationStatus.result.Duration / 1e9).toFixed(2)}s
                </span>
              </div>
            </div>
          )}

          <div className="flex flex-col gap-2">
            <Button
              variant="primary"
              size="md"
              onClick={handleStartMigration}
              disabled={migrationStatus?.status === 'running' || isLoading}
            >
              Start Migration
            </Button>
            <p className="text-xs text-text-tertiary">
              Scans the server's local <code className="font-mono">data/</code> directory and uploads flows to the database. Already-migrated flows are skipped.
            </p>
          </div>
        </div>
      </section>

      {/* Audit Log */}
      <section className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
        <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
          <ScrollText size={14} className="text-text-tertiary" />
          <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Audit Log</h2>
          <div className="ml-auto flex items-center gap-2">
            <Filter size={11} className="text-text-tertiary" />
            <select
              value={auditAction}
              onChange={e => { setAuditAction(e.target.value); fetchAudit(e.target.value) }}
              className="bg-surface-3 border border-border-default rounded text-text-primary text-xs px-2 py-1 focus:outline-none"
            >
              <option value="">All actions</option>
              <option value="auth.login">Login</option>
              <option value="auth.logout">Logout</option>
              <option value="flow.analyze">Analyze</option>
              <option value="flow.export">Export</option>
              <option value="flow.share">Share</option>
              <option value="admin.role_change">Role change</option>
              <option value="flow.version_save">Version saved</option>
            </select>
          </div>
        </div>
        <div className="overflow-x-auto">
          {auditEvents.length === 0 ? (
            <div className="px-5 py-8 text-center text-text-tertiary text-sm">No audit events recorded yet.</div>
          ) : (
            <table className="min-w-full divide-y divide-border-subtle">
              <thead className="bg-surface-3">
                <tr>
                  {['Time', 'User', 'Action', 'Resource', 'IP'].map(h => (
                    <th key={h} className="px-4 py-2 text-left text-xs font-medium text-text-tertiary uppercase tracking-wider">
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody className="divide-y divide-border-subtle">
                {auditEvents.map(ev => (
                  <tr key={ev.id} className="bg-surface-2 hover:bg-surface-3 transition-colors">
                    <td className="px-4 py-2 text-xs text-text-tertiary font-mono whitespace-nowrap">
                      {new Date(ev.createdAt).toLocaleString()}
                    </td>
                    <td className="px-4 py-2 text-xs text-text-primary whitespace-nowrap">{ev.email || ev.userId}</td>
                    <td className="px-4 py-2 whitespace-nowrap">
                      <span className={clsx(
                        'text-2xs font-mono px-1.5 py-0.5 rounded',
                        ev.action.startsWith('auth.') ? 'bg-blue-500/10 text-blue-400' :
                        ev.action.startsWith('admin.') ? 'bg-red-500/10 text-red-400' :
                        'bg-surface-3 text-text-secondary'
                      )}>
                        {ev.action}
                      </span>
                    </td>
                    <td className="px-4 py-2 text-xs text-text-tertiary font-mono whitespace-nowrap">
                      {ev.resourceType ? `${ev.resourceType}/${ev.resourceId.slice(0, 8)}` : '—'}
                    </td>
                    <td className="px-4 py-2 text-xs text-text-tertiary font-mono whitespace-nowrap">{ev.ip || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </section>

      {/* Error */}
      {error && (
        <div className="bg-semantic-error/10 border border-semantic-error/30 rounded-lg px-4 py-3 text-semantic-error text-sm flex items-start gap-2">
          <XCircle size={15} className="mt-0.5 flex-shrink-0" />
          <div>
            <p className="font-semibold">Error</p>
            <p className="text-semantic-error/80 mt-0.5">{error}</p>
          </div>
        </div>
      )}
    </div>
  )
}

function StatCard({ icon: Icon, label, value, color }: { icon: React.ElementType; label: string; value: number; color: string }) {
  return (
    <div className="bg-surface-2 border border-border-default rounded-xl p-4 flex flex-col gap-2">
      <div className="flex items-center gap-1.5">
        <Icon size={13} className={color} />
        <span className="text-xs text-text-tertiary uppercase tracking-wide">{label}</span>
      </div>
      <span className="text-3xl font-bold text-text-primary">{value}</span>
    </div>
  )
}

function MigStat({ label, value }: { label: string; value: number }) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs text-text-tertiary">{label}</span>
      <span className="text-sm font-semibold text-text-primary">{value}</span>
    </div>
  )
}
