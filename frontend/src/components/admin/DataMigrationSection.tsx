import React from 'react'
import {useTranslation} from 'react-i18next'
import {CheckCircle2, DatabaseZap, XCircle} from 'lucide-react'
import clsx from 'clsx'
import {type MigrationStatus} from '@/api/admin'
import Button from '@/components/shared/Button'
import {SectionCard, SectionNotice} from '@/components/auth/SectionCard'

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

const PHASE_KEY = {
  'not configured': 'migration.phase.notConfigured',
  running: 'migration.phase.running',
  failed: 'migration.phase.failed',
  completed: 'migration.phase.completed',
  idle: 'migration.phase.idle',
} as const

export const DataMigrationSection: React.FC<{
  status: MigrationStatus | null
  isLoading: boolean
  isStarting?: boolean
  onStart: () => void
}> = ({status, isLoading, isStarting = false, onStart}) => {
  const {t} = useTranslation('admin')
  const phase = migrationPhase(status)
  const result = status?.result
  return (
    <SectionCard
      id="admin-migration"
      icon={DatabaseZap}
      title={t('migration.title')}
      description={t('migration.subtitle')}
      action={
        phase ? (
          <span className={clsx('rounded-full px-2.5 py-0.5 text-xs font-semibold uppercase', phaseClass(phase))}>
            {t(PHASE_KEY[phase])}
          </span>
        ) : undefined
      }
    >
      <div className="space-y-4">
        {status?.error && <SectionNotice message={status.error} />}
        {result && (
          <div className="grid grid-cols-2 gap-3 rounded-lg border border-border-subtle bg-surface-3 p-3 sm:grid-cols-4">
            <Figure label={t('migration.flowsMigrated')} value={result.FlowsMigrated} />
            <Figure label={t('migration.skipped')} value={result.FlowsSkipped} />
            <Figure label={t('migration.failed')} value={result.FlowsFailed} />
            <Figure label={t('migration.conversations')} value={result.ConversationsMigrated ?? 0} />
            <div className="flex flex-col gap-1">
              <span className="text-xs text-text-tertiary">{t('migration.settings')}</span>
              <span className="flex items-center gap-1">
                {result.SettingsMoved ? (
                  <CheckCircle2 size={14} className="text-semantic-success" aria-hidden="true" />
                ) : (
                  <XCircle size={14} className="text-semantic-error" aria-hidden="true" />
                )}
                <span className="text-sm font-semibold text-text-primary">
                  {result.SettingsMoved ? t('migration.moved') : t('migration.notMoved')}
                </span>
              </span>
            </div>
            <div className="col-span-full flex items-center gap-2 border-t border-border-subtle pt-2">
              <span className="text-xs text-text-tertiary">{t('migration.duration')}</span>
              <span className="text-sm font-semibold tabular-nums text-text-primary">
                {(result.Duration / 1e9).toFixed(2)}s
              </span>
            </div>
            {result.FlowsAdopted > 0 && (
              <p className="col-span-full text-xs text-text-tertiary">
                {t('migration.adopted', {count: result.FlowsAdopted})}
              </p>
            )}
            {result.NotMigrated && result.NotMigrated.length > 0 && (
              <p className="col-span-full text-xs text-text-tertiary">
                {t('migration.notMigrated', {items: result.NotMigrated.join(', ')})}
              </p>
            )}
          </div>
        )}

        <div className="flex flex-col gap-2">
          <div>
            <Button
              variant="primary"
              size="md"
              onClick={onStart}
              disabled={!status?.configured || status.running || isLoading || isStarting}
              loading={isStarting}
            >
              {t('migration.start')}
            </Button>
          </div>
          <p className="text-xs text-text-tertiary">
            {status && !status.configured ? (
              <>
                {t('migration.notConfiguredBefore')} <code className="font-mono">PAD_DATA_DIR</code>{' '}
                {t('migration.notConfiguredMiddle')} <code className="font-mono">PAD_STORAGE=database</code>.
              </>
            ) : (
              t('migration.help')
            )}
          </p>
        </div>
      </div>
    </SectionCard>
  )
}

function Figure({label, value}: {label: string; value: number}) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs text-text-tertiary">{label}</span>
      <span className="text-sm font-semibold tabular-nums text-text-primary">{value}</span>
    </div>
  )
}
