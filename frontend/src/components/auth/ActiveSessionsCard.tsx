import React from 'react'
import {Monitor, XCircle, Trash2} from 'lucide-react'
import {useSessions} from './useSessions'
import Button from '@/components/shared/Button'
import {describeUserAgent} from '@/lib/userAgent'

function formatDate(value: string) {
  try {
    return new Date(value).toLocaleString()
  } catch {
    return value
  }
}

export const ActiveSessionsCard: React.FC = () => {
  const {sessions, loading, error, revokingId, revoke, currentSessionId, revokingOthers, revokeOthers} = useSessions()
  // Only offer bulk sign-out when we can tell which session is this one —
  // with a null currentSessionId "others" would include the current session.
  const hasOtherSessions = !!currentSessionId && sessions.some(s => s.id !== currentSessionId)

  return (
    <div className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
      <div className="px-5 py-3 border-b border-border-subtle flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <Monitor size={13} className="text-text-tertiary" />
          <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Active Sessions</h2>
        </div>
        {hasOtherSessions && (
          <Button variant="ghost" size="sm" onClick={revokeOthers} loading={revokingOthers}>
            Sign out other sessions
          </Button>
        )}
      </div>

      <div className="p-5 space-y-2">
        {error && (
          <div className="flex items-start gap-2 rounded-lg px-3 py-2.5 text-sm border bg-semantic-error/10 border-semantic-error/30 text-semantic-error">
            <XCircle size={14} className="mt-0.5 flex-shrink-0" />
            <span>{error}</span>
          </div>
        )}

        {loading ? (
          <p className="text-sm text-text-tertiary">Loading sessions...</p>
        ) : sessions.length === 0 ? (
          <p className="text-sm text-text-tertiary">No active sessions found.</p>
        ) : (
          <ul className="space-y-2">
            {sessions.map(session => {
              const isCurrent = session.id === currentSessionId
              return (
                <li
                  key={session.id}
                  className="flex items-center justify-between gap-3 rounded-lg bg-surface-3 px-3 py-2.5"
                >
                  <div className="min-w-0">
                    <p className="text-sm text-text-primary flex items-center gap-2">
                      {describeUserAgent(session.userAgent)}
                      {isCurrent && (
                        <span className="px-1.5 py-0.5 rounded text-2xs font-semibold uppercase bg-semantic-success/10 text-semantic-success">
                          This device
                        </span>
                      )}
                    </p>
                    <p className="text-xs text-text-tertiary">
                      Signed in {formatDate(session.createdAt)}
                      {session.ip && ` · ${session.ip}`}
                      {' · '}Expires {formatDate(session.expiresAt)}
                    </p>
                  </div>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => revoke(session.id)}
                    loading={revokingId === session.id}
                    disabled={isCurrent}
                    title={isCurrent ? 'This is your current session' : 'Revoke session'}
                  >
                    <Trash2 size={14} />
                  </Button>
                </li>
              )
            })}
          </ul>
        )}
      </div>
    </div>
  )
}
