import React from 'react'
import {CheckCircle2, XCircle} from 'lucide-react'
import clsx from 'clsx'
import {type MigrationStatus} from '@/api/admin'
import Button from '@/components/shared/Button'

type Phase = 'not configured' | 'running' | 'failed' | 'completed' | 'idle'

// migrationPhase derives the badge from the server's status. The panel used to
// read a `status` field the server never sends, so no state was ever shown.
export function migrationPhase(status: MigrationStatus | null): Phase | null {
  if (!status) return null
  if (!status.configured) return 'not configured'
  if (status.running) return 'running'
  if (status.error) return 'failed'
  if (status.result) return 'completed'
  return 'idle'
}

function phaseClass(phase: Phase) {
  switch (phase) {
    case 'running':
      return 'bg-semantic-info/10 text-semantic-info'
    case 'completed':
      return 'bg-semantic-success/10 text-semantic-success'
    case 'failed':
      return 'bg-semantic-error/10 text-semantic-error'
    default:
      return 'bg-surface-4 text-text-tertiary'
  }
}

export const DataMigrationSection: React.FC<{
  status: MigrationStatus | null
  isLoading: boolean
  isStarting?: boolean
  onStart: () => void
}> = ({status, isLoading, isStarting = false, onStart}) => {
  const phase = migrationPhase(status)
  const result = status?.result
  return (
    <section className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
      <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
        <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Data Migration</h2>
        <span className="text-xs text-text-tertiary">Legacy filesystem server → database</span>
        {phase && (
          <span
            className={clsx('ml-auto px-2.5 py-0.5 rounded-full text-xs font-semibold uppercase', phaseClass(phase))}
          >
            {phase}
          </span>
        )}
      </div>

      <div className="p-5 space-y-4">
        {status?.error && (
          <p className="text-sm text-semantic-error" role="alert">
            {status.error}
          </p>
        )}
        {result && (
          <div className="grid grid-cols-2 md:grid-cols-4 gap-3 bg-surface-3 border border-border-subtle rounded-lg p-3">
            <div className="flex flex-col gap-1">
              <span className="text-xs text-text-tertiary">Flows migrated</span>
              <span className="text-sm font-semibold text-text-primary tabular-nums">{result.FlowsMigrated}</span>
            </div>
            <div className="flex flex-col gap-1">
              <span className="text-xs text-text-tertiary">Skipped</span>
              <span className="text-sm font-semibold text-text-primary tabular-nums">{result.FlowsSkipped}</span>
            </div>
            <div className="flex flex-col gap-1">
              <span className="text-xs text-text-tertiary">Failed</span>
              <span className="text-sm font-semibold text-text-primary tabular-nums">{result.FlowsFailed}</span>
            </div>
            <div className="flex flex-col gap-1">
              <span className="text-xs text-text-tertiary">Conversations</span>
              <span className="text-sm font-semibold text-text-primary tabular-nums">
                {result.ConversationsMigrated ?? 0}
              </span>
            </div>
            <div className="flex flex-col gap-1">
              <span className="text-xs text-text-tertiary">Settings</span>
              <span className="flex items-center gap-1">
                {result.SettingsMoved ? (
                  <CheckCircle2 size={14} className="text-semantic-success" />
                ) : (
                  <XCircle size={14} className="text-semantic-error" />
                )}
                <span className="text-sm font-semibold text-text-primary">
                  {result.SettingsMoved ? 'Moved' : 'Not moved'}
                </span>
              </span>
            </div>
            <div className="col-span-full pt-2 border-t border-border-subtle flex items-center gap-2">
              <span className="text-xs text-text-tertiary">Duration</span>
              <span className="text-sm font-semibold text-text-primary tabular-nums">
                {(result.Duration / 1e9).toFixed(2)}s
              </span>
            </div>
            {result.NotMigrated && result.NotMigrated.length > 0 && (
              <p className="col-span-full text-xs text-text-tertiary">Not migrated: {result.NotMigrated.join(', ')}.</p>
            )}
          </div>
        )}

        <div className="flex flex-col gap-2">
          <Button
            variant="primary"
            size="md"
            onClick={onStart}
            disabled={!status?.configured || status.running || isLoading || isStarting}
            loading={isStarting}
          >
            Start Migration
          </Button>
          <p className="text-xs text-text-tertiary">
            {status && !status.configured ? (
              <>
                Not configured. Set <code className="font-mono">PAD_DATA_DIR</code> to the data directory of a server that
                ran with filesystem storage, with <code className="font-mono">PAD_STORAGE=database</code>.
              </>
            ) : (
              <>
                Copies flows, settings and chat conversations from the legacy data directory into the database. Already
                migrated flows are skipped; users, organisations, triage, versions and tokens are not migrated.
              </>
            )}
          </p>
        </div>
      </div>
    </section>
  )
}
