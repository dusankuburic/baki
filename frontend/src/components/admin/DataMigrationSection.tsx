import React from 'react'
import {CheckCircle2, XCircle} from 'lucide-react'
import clsx from 'clsx'
import {type MigrationStatus} from '@/api/admin'
import Button from '@/components/shared/Button'

function migrationStatusClass(status?: string) {
  switch (status) {
    case 'running':
      return 'bg-semantic-info/10 text-semantic-info'
    case 'completed':
      return 'bg-semantic-success/10 text-semantic-success'
    default:
      return 'bg-surface-4 text-text-tertiary'
  }
}

export const DataMigrationSection: React.FC<{
  status: MigrationStatus | null
  isLoading: boolean
  isStarting?: boolean
  onStart: () => void
}> = ({status, isLoading, isStarting = false, onStart}) => (
  <section className="bg-surface-2 border border-border-default rounded-xl overflow-hidden">
    <div className="px-5 py-3 border-b border-border-subtle flex items-center gap-2">
      <h2 className="text-xs font-semibold text-text-primary uppercase tracking-wide">Data Migration</h2>
      <span className="text-xs text-text-tertiary">Local → Cloud</span>
      {status?.status && (
        <span
          className={clsx(
            'ml-auto px-2.5 py-0.5 rounded-full text-xs font-semibold uppercase',
            migrationStatusClass(status.status),
          )}
        >
          {status.status}
        </span>
      )}
    </div>

    <div className="p-5 space-y-4">
      {status?.result && (
        <div className="grid grid-cols-2 md:grid-cols-4 gap-3 bg-surface-3 border border-border-subtle rounded-lg p-3">
          <div className="flex flex-col gap-1">
            <span className="text-xs text-text-tertiary">Migrated</span>
            <span className="text-sm font-semibold text-text-primary tabular-nums">{status.result.FlowsMigrated}</span>
          </div>
          <div className="flex flex-col gap-1">
            <span className="text-xs text-text-tertiary">Skipped</span>
            <span className="text-sm font-semibold text-text-primary tabular-nums">{status.result.FlowsSkipped}</span>
          </div>
          <div className="flex flex-col gap-1">
            <span className="text-xs text-text-tertiary">Failed</span>
            <span className="text-sm font-semibold text-text-primary tabular-nums">{status.result.FlowsFailed}</span>
          </div>
          <div className="flex flex-col gap-1">
            <span className="text-xs text-text-tertiary">Settings</span>
            <span className="flex items-center gap-1">
              {status.result.SettingsMoved ? (
                <CheckCircle2 size={14} className="text-semantic-success" />
              ) : (
                <XCircle size={14} className="text-semantic-error" />
              )}
              <span className="text-sm font-semibold text-text-primary">
                {status.result.SettingsMoved ? 'Moved' : 'Not moved'}
              </span>
            </span>
          </div>
          <div className="col-span-full pt-2 border-t border-border-subtle flex items-center gap-2">
            <span className="text-xs text-text-tertiary">Duration</span>
            <span className="text-sm font-semibold text-text-primary tabular-nums">
              {(status.result.Duration / 1e9).toFixed(2)}s
            </span>
          </div>
        </div>
      )}

      <div className="flex flex-col gap-2">
        <Button
          variant="primary"
          size="md"
          onClick={onStart}
          disabled={status?.status === 'running' || isLoading || isStarting}
          loading={isStarting}
        >
          Start Migration
        </Button>
        <p className="text-xs text-text-tertiary">
          Scans the server's local <code className="font-mono">data/</code> directory and uploads flows to the database.
          Already-migrated flows are skipped.
        </p>
      </div>
    </div>
  </section>
)
