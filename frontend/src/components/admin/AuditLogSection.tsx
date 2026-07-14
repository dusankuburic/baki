import React from 'react'
import {ScrollText, Filter} from 'lucide-react'
import clsx from 'clsx'
import {type AuditEvent} from '@/api/admin'

export const AuditLogSection: React.FC<{
  events: AuditEvent[]
  action: string
  onFilterChange: (action: string) => void
}> = ({events, action, onFilterChange}) => (
  <section className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
    <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
      <ScrollText size={14} className="text-text-tertiary" />
      <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Audit Log</h2>
      <div className="ml-auto flex items-center gap-2">
        <Filter size={11} className="text-text-tertiary" />
        <select
          value={action}
          onChange={e => onFilterChange(e.target.value)}
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
      {events.length === 0 ? (
        <div className="px-5 py-8 text-center text-text-tertiary text-sm">No audit events recorded yet.</div>
      ) : (
        <table className="min-w-full divide-y divide-border-subtle">
          <thead className="bg-surface-3">
            <tr>
              {['Time', 'User', 'Action', 'Resource', 'IP'].map(h => (
                <th
                  key={h}
                  className="px-4 py-2 text-left text-xs font-medium text-text-tertiary uppercase tracking-wider"
                >
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-border-subtle">
            {events.map(ev => (
              <tr key={ev.id} className="bg-surface-2 hover:bg-surface-3 transition-colors">
                <td className="px-4 py-2 text-xs text-text-tertiary font-mono whitespace-nowrap">
                  {new Date(ev.createdAt).toLocaleString()}
                </td>
                <td className="px-4 py-2 text-xs text-text-primary whitespace-nowrap">{ev.email || ev.userId}</td>
                <td className="px-4 py-2 whitespace-nowrap">
                  <span
                    className={clsx(
                      'text-2xs font-mono px-1.5 py-0.5 rounded',
                      ev.action.startsWith('auth.')
                        ? 'bg-blue-500/10 text-blue-400'
                        : ev.action.startsWith('admin.')
                          ? 'bg-red-500/10 text-red-400'
                          : 'bg-surface-3 text-text-secondary',
                    )}
                  >
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
)
