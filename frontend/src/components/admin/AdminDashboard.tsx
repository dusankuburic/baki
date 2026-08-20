import React from 'react'
import {Shield, Users, RefreshCw, Loader, XCircle} from 'lucide-react'
import clsx from 'clsx'
import {useAuthStore} from '@/stores/authStore'
import Button from '@/components/shared/Button'
import {useAdminData} from './useAdminData'
import {UserManagementSection} from './UserManagementSection'
import {DataMigrationSection} from './DataMigrationSection'
import {AuditLogSection} from './AuditLogSection'

function StatCard({
  icon: Icon,
  label,
  value,
  color,
}: {
  icon: React.ElementType
  label: string
  value: number
  color: string
}) {
  return (
    <div className="bg-surface-2 border border-border-default rounded-xl p-4 flex flex-col gap-2">
      <div className="flex items-center gap-1.5">
        <Icon size={13} className={color} />
        <span className="text-xs text-text-tertiary uppercase tracking-wide">{label}</span>
      </div>
      <span className="text-3xl font-bold text-text-primary tabular-nums">{value}</span>
    </div>
  )
}

export const AdminDashboard: React.FC = () => {
  const currentUser = useAuthStore(s => s.user)
  const isAdmin = currentUser?.role === 'admin'

  const {
    migrationStatus,
    users,
    auditEvents,
    auditAction,
    isLoading,
    isStarting,
    error,
    fetchAll,
    startMigration,
    changeRole,
    filterAudit,
  } = useAdminData(isAdmin)

  if (!isAdmin) {
    return (
      <div className="p-6 flex items-center gap-2 text-semantic-error text-sm">
        <Shield size={16} />
        <span>Access Denied: Administrator role required.</span>
      </div>
    )
  }

  const adminCount = users.filter(u => u.role === 'admin').length
  const memberCount = users.filter(u => u.role === 'member').length
  const otherCount = users.filter(u => u.role !== 'admin' && u.role !== 'member').length

  return (
    <div className="p-6 md:p-8 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-text-primary">Admin Dashboard</h1>
          <p className="text-sm text-text-tertiary mt-0.5">Manage users and system settings</p>
        </div>
        <Button
          variant="ghost"
          size="sm"
          icon={isLoading ? Loader : RefreshCw}
          onClick={fetchAll}
          disabled={isLoading}
          className={clsx(isLoading && '[&_svg]:animate-spin')}
        >
          {isLoading ? 'Refreshing…' : 'Refresh'}
        </Button>
      </div>

      <div className="grid grid-cols-2 md:grid-cols-4 gap-3">
        <StatCard icon={Users} label="Total Users" value={users.length} color="text-text-secondary" />
        <StatCard icon={Shield} label="Admins" value={adminCount} color="text-block-subflow" />
        <StatCard icon={Users} label="Members" value={memberCount} color="text-block-action" />
        <StatCard icon={Users} label="Other" value={otherCount} color="text-text-tertiary" />
      </div>

      <UserManagementSection users={users} onRoleChange={(userId, role) => changeRole(userId, role, currentUser?.id)} />
      <DataMigrationSection
        status={migrationStatus}
        isLoading={isLoading}
        isStarting={isStarting}
        onStart={startMigration}
      />
      <AuditLogSection events={auditEvents} action={auditAction} onFilterChange={filterAudit} />

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
