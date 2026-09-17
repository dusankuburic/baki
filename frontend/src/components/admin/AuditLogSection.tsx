import React, {useState} from 'react'
import {ScrollText, Filter, ChevronLeft, ChevronRight} from 'lucide-react'
import clsx from 'clsx'
import {type AuditEvent} from '@/api/admin'

// The action list comes from GET /api/admin/audit/actions (the server's closed
// set) instead of a hand-maintained <option> list: the old list offered 7 of
// 35 actions, leaving token minting, account deletion, failed logins and every
// org.* event unfilterable. Paging and the user filter map onto the backend's
// existing offset/limit/userId query parameters.
export const AuditLogSection: React.FC<{
  events: AuditEvent[]
  action: string
  userId: string
  actions: string[]
  offset: number
  pageSize: number
  onFilterChange: (action: string, userId?: string) => void
  onPage: (direction: 'prev' | 'next') => void
}> = ({events, action, userId, actions, offset, pageSize, onFilterChange, onPage}) => {
  const [userDraft, setUserDraft] = useState(userId)
  const page = Math.floor(offset / pageSize) + 1
  // The list endpoint has no total; a full page is the only "there may be
  // more" signal, so Next stays enabled exactly when this page is full.
  const hasNext = events.length >= pageSize

  return (
    <section className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
      <div className="px-5 py-3 border-b border-border-subtle flex flex-wrap items-center gap-2">
        <ScrollText size={14} className="text-text-tertiary" />
        <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Audit Log</h2>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <Filter size={11} className="text-text-tertiary" />
          <select
            value={action}
            onChange={e => onFilterChange(e.target.value)}
            aria-label="Filter by action"
            className="bg-surface-3 border border-border-default rounded text-text-primary text-xs px-2 py-1 focus:outline-none"
          >
            <option value="">All actions</option>
            {actions.map(a => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
          <input
            type="text"
            value={userDraft}
            onChange={e => setUserDraft(e.target.value)}
            onBlur={() => {
              if (userDraft !== userId) onFilterChange(action, userDraft.trim())
            }}
            onKeyDown={e => {
              if (e.key === 'Enter') onFilterChange(action, userDraft.trim())
            }}
            placeholder="User ID"
            aria-label="Filter by user ID"
            className="w-36 bg-surface-3 border border-border-default rounded text-text-primary text-xs px-2 py-1 focus:outline-none"
          />
        </div>
      </div>
      <div className="overflow-x-auto">
        {events.length === 0 ? (
          <div className="px-5 py-8 text-center text-text-tertiary text-sm">
            {offset > 0 ? 'No more audit events.' : 'No audit events recorded yet.'}
          </div>
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
      <div className="px-5 py-2 border-t border-border-subtle flex items-center justify-between text-xs text-text-tertiary">
        <span>
          Page {page}
          {events.length > 0 ? ` · rows ${offset + 1}–${offset + events.length}` : ''}
        </span>
        <div className="flex items-center gap-1">
          <button
            type="button"
            onClick={() => onPage('prev')}
            disabled={offset === 0}
            aria-label="Previous page"
            className="p-1 rounded hover:bg-surface-3 disabled:opacity-30 transition-colors"
          >
            <ChevronLeft size={14} />
          </button>
          <button
            type="button"
            onClick={() => onPage('next')}
            disabled={!hasNext}
            aria-label="Next page"
            className="p-1 rounded hover:bg-surface-3 disabled:opacity-30 transition-colors"
          >
            <ChevronRight size={14} />
          </button>
        </div>
      </div>
    </section>
  )
}
