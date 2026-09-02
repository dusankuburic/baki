import {useCallback, useState} from 'react'
import {Activity, Database, HardDrive, RefreshCw, Server, Wifi, WifiOff} from 'lucide-react'
import {adminApi, type SystemHealth} from '@/api/admin'
import {Button, useToast} from '@/components/shared'
import {logger} from '@/lib/logger'
import clsx from 'clsx'

// OperationsSection (R2-6): the admin console's ops surface — per-subsystem
// health, connector status, and one-click scanner/ingester triggers. These
// endpoints existed with NO frontend consumer; admins had to curl with a JWT.

interface Props {
  visible: boolean
}

export function OperationsSection({visible}: Props) {
  const toast = useToast()
  const [health, setHealth] = useState<SystemHealth | null>(null)
  const [loading, setLoading] = useState(false)
  const [connected, setConnected] = useState<boolean | null>(null)
  const [scanBusy, setScanBusy] = useState(false)
  const [ingestBusy, setIngestBusy] = useState(false)

  const loadHealth = useCallback(async () => {
    setLoading(true)
    try {
      const [h, pp] = await Promise.all([adminApi.systemHealth(), adminApi.ppStatus()])
      setHealth(h)
      setConnected(pp?.connected ?? null)
    } catch (e) {
      logger.warn(e)
      setHealth(null)
    } finally {
      setLoading(false)
    }
  }, [])

  // Load when the section becomes visible (the dashboard is lazy-loaded).
  const [loadedOnce, setLoadedOnce] = useState(false)
  if (visible && !loadedOnce) {
    setLoadedOnce(true)
    void loadHealth()
  }

  const trigger = useCallback(
    async (what: 'scan' | 'ingest') => {
      const setBusy = what === 'scan' ? setScanBusy : setIngestBusy
      setBusy(true)
      try {
        if (what === 'scan') await adminApi.triggerScannerScan()
        else await adminApi.triggerIngesterIngest()
        toast.success(what === 'scan' ? 'Governance scan started' : 'Cloud ingest started')
      } catch (e) {
        toast.error('Trigger failed', {description: String(e)})
      } finally {
        setBusy(false)
      }
    },
    [toast],
  )

  return (
    <section className="border border-border-default rounded-xl p-4 bg-surface-1" data-testid="operations-section">
      <div className="flex items-center justify-between mb-3">
        <div className="flex items-center gap-2">
          <Activity size={14} className="text-text-tertiary" />
          <h3 className="text-xs font-bold uppercase tracking-wider text-text-tertiary">Operations</h3>
        </div>
        <Button variant="ghost" size="sm" icon={RefreshCw} onClick={() => void loadHealth()} disabled={loading}>
          {loading ? 'Checking…' : 'Refresh'}
        </Button>
      </div>

      {/* Subsystem health */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-2 mb-3">
        <HealthTile icon={Database} label="Database" comp={health?.database} />
        <HealthTile icon={HardDrive} label="Blob storage" comp={health?.blob} />
        <HealthTile icon={Server} label="Redis" comp={health?.redis} />
        <HealthTile
          icon={connected ? Wifi : WifiOff}
          label="Power Platform"
          comp={connected === null ? undefined : {status: connected ? 'ok' : 'error', error: connected ? undefined : 'not connected'}}
        />
      </div>
      {health && (
        <p className={clsx('text-2xs mb-3', health.overall === 'ok' ? 'text-semantic-success' : health.overall === 'degraded' ? 'text-semantic-warning' : 'text-semantic-error')}>
          Overall: {health.overall}
        </p>
      )}

      {/* One-click triggers */}
      <div className="flex flex-wrap gap-2">
        <Button variant="secondary" size="sm" icon={RefreshCw} onClick={() => void trigger('scan')} disabled={scanBusy}>
          {scanBusy ? 'Starting…' : 'Run governance scan'}
        </Button>
        <Button variant="secondary" size="sm" icon={RefreshCw} onClick={() => void trigger('ingest')} disabled={ingestBusy}>
          {ingestBusy ? 'Starting…' : 'Ingest cloud flows'}
        </Button>
      </div>
    </section>
  )
}

function HealthTile({icon: Icon, label, comp}: {icon: typeof Database; label: string; comp?: {status: string; error?: string}}) {
  const status = comp?.status ?? 'unknown'
  const tone =
    status === 'ok' || status === 'skipped'
      ? status === 'ok'
        ? 'text-semantic-success'
        : 'text-text-tertiary/60'
      : status === 'error'
        ? 'text-semantic-error'
        : 'text-text-tertiary/40'
  return (
    <div className="flex items-center gap-2 px-2.5 py-2 rounded-lg border border-border-subtle bg-surface-2" title={comp?.error ?? label}>
      <Icon size={13} className={tone} />
      <div className="min-w-0">
        <div className="text-xs text-text-secondary truncate">{label}</div>
        <div className={clsx('text-2xs capitalize', tone)}>{status}</div>
      </div>
    </div>
  )
}
