import React from 'react'
import {Users} from 'lucide-react'
import clsx from 'clsx'
import {type AuthUser} from '@/api/auth'
import {roleBadgeClass} from '@/lib/roleBadge'
import {useConfirm} from '@/components/shared'

export const UserManagementSection: React.FC<{
  users: AuthUser[]
  onRoleChange: (userId: string, newRole: string) => void
}> = ({users, onRoleChange}) => {
  const {confirm} = useConfirm()
  return (
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
              <th
                key={h}
                className="px-5 py-2.5 text-left text-xs font-medium text-text-tertiary uppercase tracking-wider"
              >
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-border-subtle">
          {users.map(u => (
            <tr key={u.id} className="bg-surface-2 hover:bg-surface-3 transition-colors duration-fast">
              <td className="px-5 py-3 text-sm font-medium text-text-primary max-w-[200px]">
                <span className="block truncate">{u.email}</span>
              </td>
              <td className="px-5 py-3 text-xs text-text-tertiary font-mono max-w-[120px] truncate">{u.id}</td>
              <td className="px-5 py-3 whitespace-nowrap">
                <span
                  className={clsx('px-2 py-0.5 rounded-md text-xs font-semibold uppercase', roleBadgeClass(u.role))}
                >
                  {u.role}
                </span>
              </td>
              <td className="px-5 py-3 whitespace-nowrap">
                <select
                  value={u.role}
                  onChange={e => {
                    if (e.target.value === u.role) return
                    // Destructive-parity confirm (U4.3): one accidental
                    // scroll-click used to change a user's role instantly.
                    void (async () => {
                      const ok = await confirm({
                        title: 'Change role',
                        message: `Change ${u.email || u.id} from "${u.role}" to "${e.target.value}"?`,
                        confirmLabel: 'Change role',
                      })
                      if (ok) onRoleChange(u.id, e.target.value)
                    })()
                  }}
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
  )
}
