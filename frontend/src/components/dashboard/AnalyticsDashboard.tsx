import {useCallback, useEffect, useMemo, useRef, useState} from 'react'
import clsx from 'clsx'
import {
  BarChart3, RefreshCw, FolderSearch, Download, AlertTriangle,
  ShieldAlert, Activity, FileWarning, ArrowLeft,
} from 'lucide-react'
import {analysisApi} from '@/api'
import {logger} from '@/lib/logger'
import {createAdapter} from '@/platform/adapters'
import {useSystemStore} from '@/stores/systemStore'
import {useToast} from '@/components/shared'
import {csvCell, downloadBlob} from '@/lib/csv'
import {scoreColor} from '@/lib/scoring'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import type {BatchAnalysis, DashboardStats} from '@/types'

function StatCard({label, value, accent}: {label: string; value: string | number; accent?: string}) {
  return (
    <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
      <div className="text-sm text-text-tertiary uppercase tracking-widest mb-1">{label}</div>
      <div className={clsx('text-2xl font-black font-mono tabular-nums', accent ?? 'text-text-primary')}>{value}</div>
    </div>
  )
}

const sevChip: Record<string, string> = {
  error: 'text-red-400 bg-red-500/10',
  warning: 'text-amber-400 bg-amber-500/10',
  info: 'text-blue-400 bg-blue-500/10',
}

const catChip: Record<string, string> = {
  Security: 'text-red-400 bg-red-500/10',
  Reliability: 'text-amber-400 bg-amber-500/10',
  Performance: 'text-orange-400 bg-orange-500/10',
  Style: 'text-purple-400 bg-purple-500/10',
  Logic: 'text-cyan-400 bg-cyan-500/10',
}

function exportBatchCSV(batch: BatchAnalysis) {
  const rows = [['Flow', 'Errors', 'Warnings', 'Info', 'Health', 'Load Error']]
  for (const r of batch.results) {
    rows.push([
      csvCell(r.flowName),
      String(r.report?.stats.errors ?? ''),
      String(r.report?.stats.warnings ?? ''),
      String(r.report?.stats.info ?? ''),
      String(r.report?.metrics?.healthScore ?? ''),
      r.error ? csvCell(r.error) : '',
    ])
  }
  const csv = rows.map(r => r.join(',')).join('\n')
  downloadBlob(csv, 'text/csv;charset=utf-8;', `batch-analysis-${new Date().toISOString().slice(0, 10)}.csv`)
}

// AnalyticsDashboard surfaces the session-wide analysis aggregates plus
// folder-level batch analysis with per-file error rows.
export default function AnalyticsDashboard() {
  const toast = useToast()
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const sessionAnalyticsEnabled = useSystemStore(s => s.info?.capabilities.sessionAnalytics ?? false)
  const isLoaded = useSystemStore(s => s.isLoaded)

  // Loading a new flow/folder clears the server-side session cache, so re-fetch
  // when the open document changes to reflect only the current working context.
  const docId = useFlowStore(s => s.document?.id ?? null)
  const [stats, setStats] = useState<DashboardStats | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [batch, setBatch] = useState<BatchAnalysis | null>(null)
  const [batchRunning, setBatchRunning] = useState(false)
  const reqIdRef = useRef(0)
  const hasStatsRef = useRef(false)
  const toastRef = useRef(toast)
  toastRef.current = toast

  const refresh = useCallback((background = false) => {
    // Session analytics aggregate the app's in-process analyzer cache and read
    // server-local folders. In cloud/JWT mode the backend returns 403 (data
    // would otherwise span tenants), so we check the capability flag provided
    // by the backend. The Home dashboard is the cloud view. Wait for the
    // capability to load before fetching so we don't fire a doomed 403 in
    // cloud mode; the effect re-runs once isLoaded flips true.
    if (!isLoaded || !sessionAnalyticsEnabled) return

    reqIdRef.current++
    const myReq = reqIdRef.current
    if (!background) {
      setLoading(true)
      setError(null)
    }
    analysisApi.getDashboard()
      .then(s => {
        if (myReq !== reqIdRef.current) return
        setStats(s)
        hasStatsRef.current = true
        setError(null)
      })
      .catch((err) => {
        if (myReq !== reqIdRef.current) return
        logger.warn('Failed to load dashboard stats', err)
        if (hasStatsRef.current) {
          toastRef.current.error('Dashboard refresh failed', {
            description: err instanceof Error ? err.message : 'Unknown error',
          })
        } else {
          setError(err instanceof Error ? err.message : 'Failed to load')
        }
      })
      .finally(() => {
        if (myReq === reqIdRef.current) setLoading(false)
      })
  }, [sessionAnalyticsEnabled, isLoaded])

  useEffect(() => {
    refresh()
    return () => { reqIdRef.current++ }
  }, [refresh, docId])

  const handleBatch = useCallback(async () => {
    const adapter = createAdapter()
    const result = await adapter.fileOpenDirectory()
    if (!result) return
    if (typeof result !== 'string' || result.trim().startsWith('{')) {
      toast.error('Batch analysis requires a local file system')
      return
    }
    setBatchRunning(true)
    try {
      const b = await analysisApi.batchAnalyze(result)
      setBatch(b)
      setError(null)
      refresh(true)
    } catch (err) {
      toast.error('Batch analysis failed: ' + (err as Error).message)
    } finally {
      setBatchRunning(false)
    }
  }, [toast, refresh])

  const ruleEntries = useMemo(
    () => (stats ? Object.entries(stats.findingsByRule).sort((a, b) => b[1] - a[1]).slice(0, 10) : []),
    [stats],
  )
  const maxRuleCount = ruleEntries[0]?.[1] ?? 1
  const isEmpty = !stats || stats.totalFlowsAnalyzed === 0
  const sortedBatchResults = useMemo(
    () => (batch
      ? [...batch.results].sort((a, b) => (b.report?.findings.length ?? -1) - (a.report?.findings.length ?? -1))
      : []),
    [batch],
  )

  // Gated view: session analytics + folder batch are only available when the
  // backend supports them (e.g. local mode). Show a notice and point to Home.
  if (isLoaded && !sessionAnalyticsEnabled) {
    return (
      <div className="max-w-4xl mx-auto space-y-5">
        <button
          onClick={() => setMainPaneView('home')}
          className="inline-flex items-center gap-1 text-sm text-text-tertiary hover:text-text-secondary transition-colors"
        >
          <ArrowLeft size={14} /> Home
        </button>
        <div className="flex flex-col items-center justify-center gap-2 py-16 rounded-xl border border-border-subtle bg-surface-0">
          <BarChart3 size={22} className="text-text-tertiary" />
          <span className="text-sm text-text-secondary">Session analytics are not available in this mode</span>
          <span className="text-sm text-text-tertiary">Open the Home dashboard for your cloud analytics.</span>
        </div>
      </div>
    )
  }

  return (
    <div className="max-w-4xl mx-auto space-y-5">
      <button
        onClick={() => setMainPaneView('home')}
        className="inline-flex items-center gap-1 text-sm text-text-tertiary hover:text-text-secondary transition-colors"
      >
        <ArrowLeft size={14} /> Home
      </button>
      <div className="flex items-center gap-3">
        <BarChart3 size={20} className="text-brand-500" />
        <div className="flex-1">
          <h2 className="text-2xl font-bold text-text-primary">Analysis Dashboard</h2>
          <p className="text-sm text-text-tertiary">Aggregated findings across every flow analyzed this session</p>
        </div>
        <button
          onClick={() => refresh()}
          disabled={loading}
          title="Refresh"
          aria-label="Refresh dashboard"
          className="text-text-tertiary hover:text-text-secondary p-1.5 rounded hover:bg-surface-3 transition-colors disabled:opacity-50"
        >
          <RefreshCw size={14} />
        </button>
        <button
          onClick={handleBatch}
          disabled={batchRunning}
          className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-accent text-white text-sm font-medium hover:bg-accent-light transition-colors disabled:opacity-50"
        >
          <FolderSearch size={13} />
          {batchRunning ? 'Analyzing…' : 'Analyze Folder…'}
        </button>
      </div>

      {error ? (
        <div className="flex flex-col items-center justify-center gap-3 py-16 rounded-xl border border-border-subtle bg-surface-0">
          <AlertTriangle size={22} className="text-red-400" />
          <span className="text-sm text-text-secondary">Failed to load dashboard</span>
          <span className="text-sm text-text-tertiary">{error}</span>
          <button
            onClick={() => refresh()}
            className="inline-flex items-center gap-2 px-3 py-1.5 text-sm rounded-lg bg-surface-2 border border-border-subtle text-text-primary hover:bg-surface-3 transition-colors"
          >
            <RefreshCw size={14} /> Retry
          </button>
        </div>
      ) : loading ? (
        <div className="space-y-4">
          <div className="grid grid-cols-3 gap-2">
            {[0, 1, 2].map(i => (
              <div key={i} className="p-3 rounded-xl border border-border-subtle bg-surface-0">
                <div className="animate-pulse bg-surface-3 rounded h-3 w-16 mb-2" />
                <div className="animate-pulse bg-surface-3 rounded h-8 w-20" />
              </div>
            ))}
          </div>
          <div className="animate-pulse bg-surface-2 border border-border-subtle rounded-xl h-48" />
          <div className="animate-pulse bg-surface-2 border border-border-subtle rounded-xl h-48" />
        </div>
      ) : isEmpty && !batch ? (
        <div className="flex flex-col items-center justify-center gap-2 py-16 rounded-xl border border-border-subtle bg-surface-0">
          <Activity size={22} className="text-text-tertiary" />
          <span className="text-sm text-text-secondary">No analyses yet this session</span>
          <span className="text-sm text-text-tertiary">Run an analysis or batch-analyze a folder to populate the dashboard</span>
        </div>
      ) : stats && !isEmpty && (
        <>
          <div className="grid grid-cols-3 gap-2">
            <StatCard label="Flows Analyzed" value={stats.totalFlowsAnalyzed} />
            <StatCard label="Total Findings" value={stats.totalFindings} />
            <StatCard
              label="Avg Health"
              value={stats.avgHealthScore.toFixed(0)}
              accent={scoreColor(stats.avgHealthScore)}
            />
          </div>

          <div className="flex flex-wrap gap-1.5">
            {Object.entries(stats.findingsBySeverity).map(([sev, n]) => (
              <span key={sev} className={clsx('text-sm font-bold px-2 py-0.5 rounded-full', sevChip[sev] ?? 'text-text-tertiary bg-surface-3')}>
                {n} {sev}
              </span>
            ))}
            {Object.entries(stats.findingsByCategory).map(([cat, n]) => (
              <span key={cat} className={clsx('text-sm font-bold px-2 py-0.5 rounded-full', catChip[cat] ?? 'text-text-tertiary bg-surface-3')}>
                {n} {cat}
              </span>
            ))}
          </div>

          {ruleEntries.length > 0 && (
            <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
              <h3 className="text-sm font-bold uppercase tracking-widest text-text-tertiary mb-2 flex items-center gap-1.5">
                <ShieldAlert size={14} />
                Findings by Rule
              </h3>
              <div className="space-y-1.5">
                {ruleEntries.map(([rule, count]) => (
                  <div key={rule} className="flex items-center gap-2">
                    <span className="text-sm text-text-secondary font-mono w-44 truncate shrink-0">{rule}</span>
                    <div className="flex-1 h-1.5 bg-surface-3 rounded-full overflow-hidden">
                      <div className="h-full rounded-full bg-brand-500/70" style={{width: `${(count / maxRuleCount) * 100}%`}} />
                    </div>
                    <span className="text-sm text-text-tertiary tabular-nums w-8 text-right shrink-0">{count}</span>
                  </div>
                ))}
              </div>
            </div>
          )}

          {stats.topProblemFlows?.length > 0 && (
            <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
              <h3 className="text-sm font-bold uppercase tracking-widest text-text-tertiary mb-2 flex items-center gap-1.5">
                <FileWarning size={14} />
                Top Problem Flows
              </h3>
              <div className="space-y-1">
                {stats.topProblemFlows.map(p => (
                  <div key={p.flowId} className="flex items-center gap-2 px-2 py-1.5 rounded border border-border-subtle bg-surface-1">
                    <span className="text-sm text-text-primary flex-1 truncate">{p.flowName || p.flowId.slice(0, 8)}</span>
                    <span className="text-sm text-text-tertiary tabular-nums">{p.findingCount} findings</span>
                    <span className={clsx('text-sm font-bold font-mono tabular-nums', scoreColor(p.healthScore))}>{p.healthScore}</span>
                  </div>
                ))}
              </div>
            </div>
          )}
        </>
      )}

      {batch && (
        <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
          <div className="flex items-center justify-between mb-2">
            <h3 className="text-sm font-bold uppercase tracking-widest text-text-tertiary flex items-center gap-1.5">
              <FolderSearch size={14} />
              Batch Results
            </h3>
            <div className="flex items-center gap-3">
              <span className="text-sm text-text-tertiary tabular-nums">
                {batch.totalFlows} flows · <span className="text-red-400">{batch.totalErrors}E</span>{' '}
                <span className="text-amber-400">{batch.totalWarnings}W</span>{' '}
                <span className="text-blue-400">{batch.totalInfo}I</span> · avg health{' '}
                <span className={scoreColor(batch.avgHealthScore)}>{batch.avgHealthScore.toFixed(0)}</span>
              </span>
              <button
                onClick={() => exportBatchCSV(batch)}
                title="Export batch results as CSV"
                aria-label="Export batch results as CSV"
                className="text-text-tertiary hover:text-text-secondary p-1 rounded hover:bg-surface-3 transition-colors"
              >
                <Download size={12} />
              </button>
            </div>
          </div>
          <div className="space-y-1">
            {sortedBatchResults.map((r, i) => (
                <div
                  key={`${r.flowName}-${i}`}
                  className={clsx(
                    'flex items-center gap-2 px-2 py-1.5 rounded border',
                    r.error ? 'border-red-500/20 bg-red-500/5' : 'border-border-subtle bg-surface-1',
                  )}
                >
                  {r.error && <AlertTriangle size={11} className="text-red-400 shrink-0" />}
                  <span className="text-sm text-text-primary flex-1 truncate">{r.flowName}</span>
                  {r.error ? (
                    <span className="text-sm text-red-400/90 truncate max-w-[50%]">{r.error}</span>
                  ) : (
                    <>
                      <span className="text-sm tabular-nums">
                        <span className="text-red-400">{r.report?.stats.errors ?? 0}E</span>{' '}
                        <span className="text-amber-400">{r.report?.stats.warnings ?? 0}W</span>{' '}
                        <span className="text-blue-400">{r.report?.stats.info ?? 0}I</span>
                      </span>
                      <span className={clsx('text-sm font-bold font-mono tabular-nums w-7 text-right', scoreColor(r.report?.metrics?.healthScore ?? 0))}>
                        {r.report?.metrics?.healthScore ?? '—'}
                      </span>
                    </>
                  )}
                </div>
              ))}
          </div>
        </div>
      )}
    </div>
  )
}
