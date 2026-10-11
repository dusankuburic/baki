import {useCallback, useEffect, useState, type ReactNode} from 'react'
import {useTranslation} from 'react-i18next'
import clsx from 'clsx'
import {Activity, Database, HardDrive, Info, Play, RefreshCw, Server, ShieldCheck} from 'lucide-react'
import {adminApi, type ComponentStatus, type SystemHealth} from '@/api/admin'
import {Button, useToast} from '@/components/shared'
import {SectionCard, SectionNotice} from '@/components/auth/SectionCard'
import {getErrorMessage} from '@/lib/getErrorMessage'
import {useAsync} from '@/hooks/useAsync'
import {useAdminStore} from '@/stores/adminStore'
import {useSystemStore} from '@/stores/systemStore'
import {AdminSection} from './AdminSection'

interface Props {
  visible?: boolean
  refreshKey?: number
  children?: ReactNode
}

type HealthStatus = ComponentStatus['status'] | 'unknown'

export function OperationsSection({visible = true, refreshKey = 0, children}: Props) {
  const {t} = useTranslation('admin')
  const toast = useToast()
  const setHealth = useAdminStore(s => s.setHealth)
  const info = useSystemStore(s => s.info)
  const {
    data: health,
    isLoading,
    error,
    refetch,
  } = useAsync<SystemHealth | null>(
    signal => (visible ? adminApi.systemHealth(signal) : Promise.resolve(null)),
    [visible, refreshKey],
  )
  useEffect(() => {
    setHealth(health)
  }, [health, setHealth])

  const [scanBusy, setScanBusy] = useState(false)
  const trigger = useCallback(async () => {
    setScanBusy(true)
    try {
      await adminApi.triggerScannerScan()
      toast.success(t('operations.scanStarted'))
    } catch (e) {
      toast.error(t('operations.scanFailed'), {description: getErrorMessage(e)})
    } finally {
      setScanBusy(false)
    }
  }, [toast, t])

  return (
    <AdminSection id="admin-operations" title={t('sections.operations')} description={t('sectionSub.operations')}>
      <div className="space-y-4" data-testid="operations-section">
        <SectionCard
          id="admin-health"
          icon={Activity}
          title={t('operations.health')}
          description={t('operations.healthSub')}
          action={
            <>
              {health && <OverallPill status={health.overall} />}
              <Button
                variant="ghost"
                size="sm"
                icon={RefreshCw}
                onClick={refetch}
                disabled={isLoading}
                className={clsx(isLoading && '[&_svg]:animate-spin')}
              >
                {isLoading ? t('operations.checking') : t('operations.refreshHealth')}
              </Button>
            </>
          }
        >
          {error && <SectionNotice message={t('operations.healthFailed')} />}
          <div className="grid grid-cols-[repeat(auto-fit,minmax(10rem,1fr))] gap-2">
            <HealthTile icon={Database} label={t('operations.database')} comp={health?.database} />
            <HealthTile icon={HardDrive} label={t('operations.blob')} comp={health?.blob} />
            <HealthTile icon={Server} label={t('operations.redis')} comp={health?.redis} />
          </div>
          <p className="mt-3 flex items-center gap-1.5 text-2xs text-text-tertiary">
            <Info size={11} aria-hidden="true" />
            {info
              ? t('operations.about', {
                  version: info.version,
                  commit: info.gitCommit ? info.gitCommit.slice(0, 7) : '—',
                  platform: info.platform,
                  arch: info.arch,
                })
              : t('operations.aboutUnknown')}
          </p>
        </SectionCard>

        <SectionCard
          id="admin-governance"
          icon={ShieldCheck}
          title={t('operations.governance')}
          description={t('operations.governanceSub')}
          action={
            <Button variant="secondary" size="sm" icon={Play} onClick={() => void trigger()} disabled={scanBusy}>
              {scanBusy ? t('operations.scanning') : t('operations.scan')}
            </Button>
          }
        />

        {children}
      </div>
    </AdminSection>
  )
}

function OverallPill({status}: {status: SystemHealth['overall']}) {
  const {t} = useTranslation('admin')
  const tone =
    status === 'ok'
      ? 'bg-semantic-success/10 text-semantic-success'
      : status === 'degraded'
        ? 'bg-semantic-warning/10 text-semantic-warning'
        : 'bg-semantic-error/10 text-semantic-error'
  return (
    <span className={clsx('rounded-full px-2.5 py-0.5 text-2xs font-semibold', tone)}>
      {t('operations.overall', {status: t(`operations.status.${status}`)})}
    </span>
  )
}

function statusLabel(t: (key: `operations.status.${HealthStatus}`) => string, status: string): string {
  switch (status) {
    case 'ok':
    case 'error':
    case 'skipped':
      return t(`operations.status.${status}`)
    default:
      return t('operations.status.unknown')
  }
}

function HealthTile({icon: Icon, label, comp}: {icon: typeof Database; label: string; comp?: ComponentStatus}) {
  const {t} = useTranslation('admin')
  const status = comp?.status ?? 'unknown'
  const tone =
    status === 'ok'
      ? 'text-semantic-success'
      : status === 'error'
        ? 'text-semantic-error'
        : status === 'skipped'
          ? 'text-text-tertiary/60'
          : 'text-text-tertiary/40'
  return (
    <div
      className="flex items-center gap-2 rounded-lg border border-border-subtle bg-surface-3/60 px-2.5 py-2"
      title={comp?.error ?? label}
    >
      <Icon size={13} className={tone} aria-hidden="true" />
      <div className="min-w-0">
        <div className="truncate text-xs text-text-secondary">{label}</div>
        <div className={clsx('text-2xs capitalize', tone)}>{statusLabel(t, status)}</div>
      </div>
    </div>
  )
}
