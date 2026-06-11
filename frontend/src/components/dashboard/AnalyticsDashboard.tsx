import {useCallback, useEffect, useState} from 'react'
import clsx from 'clsx'
import {
  BarChart3, RefreshCw, FolderSearch, Download, AlertTriangle,
  ShieldAlert, Activity, FileWarning,
} from 'lucide-react'
import {analysisApi} from '@/api'
import {createAdapter} from '@/platform/adapters'
import {useToast} from '@/components/shared'
import type {BatchAnalysis, DashboardStats} from '@/types/domain'

function scoreColor(score: number): string {
  if (score >= 80) return 'text-green-400'
  if (score >= 60) return 'text-amber-400'
  if (score >= 40) return 'text-orange-400'
  return 'text-red-400'
}

function StatCard({label, value, accent}: {label: string; value: string | number; accent?: string}) {
  return (
    <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
      <div className="text-2xs text-text-tertiary uppercase tracking-widest mb-1">{label}</div>
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
      `"${r.flowName.replace(/"/g, '""')}"`,
      String(r.report?.stats.errors ?? ''),
      String(r.report?.stats.warnings ?? ''),
      String(r.report?.stats.info ?? ''),
      String(r.report?.metrics?.healthScore ?? ''),
      r.error ? `"${r.error.replace(/"/g, '""')}"` : '',
    ])
  }
  const blob = new Blob([rows.map(r => r.join(',')).join('\n')], {type: 'text/csv;charset=utf-8;'})
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `batch-analysis-${new Date().toISOString().slice(0, 10)}.csv`
  a.click()
  URL.revokeObjectURL(url)
}

// AnalyticsDashboard surfaces the session-wide analysis aggregates (the
// /dashboard endpoint existed unused since it was written) plus folder-level
// batch analysis with per-file error rows.
export default function AnalyticsDashboard() {
  const toast = useToast()
  const [stats, setStats] = useState<DashboardStats | null>(null)
  const [batch, setBatch] = useState<BatchAnalysis | null>(null)
  const [batchRunning, setBatchRunning] = useState(false)

  const refresh = useCallback(() => {
    analysisApi.getDashboard().then(s => setStats(s)).catch(() => {})
  }, [])

  useEffect(() => { refresh() }, [refresh])

  const handleBatch = useCallback(async () => {
    const adapter = createAdapter()
    const result = await adapter.fileOpenDirectory()
    if (!result) return
    if (typeof result !== 'string' || result.trim().startsWith('{')) {
      toast.error('Batch analysis requires the desktop app')
      return
    }
    setBatchRunning(true)
    try {
      const b = await analysisApi.batchAnalyze(result)
      setBatch(b)
      refresh()
    } catch (err) {
      toast.error('Batch analysis failed: ' + (err as Error).message)
    } finally {
      setBatchRunning(false)
    }
  }, [toast, refresh])

  const ruleEntries = stats
    ? Object.entries(stats.findingsByRule).sort((a, b) => b[1] - a[1]).slice(0, 10)
    : []
  const maxRuleCount = ruleEntries[0]?.[1] ?? 1
  const isEmpty = !stats || stats.totalFlowsAnalyzed === 0

  return (
    <div className="max-w-3xl mx-auto space-y-5">
      <div className="flex items-center gap-3">
        <BarChart3 size={20} className="text-brand-500" />
        <div className="flex-1">
          <h2 className="text-xl font-semibold text-text-primary">Analysis Dashboard</h2>
          <p className="text-xs text-text-tertiary">Aggregated findings across every flow analyzed this session</p>
        </div>
        <button
          onClick={refresh}
          title="Refresh"
          aria-label="Refresh dashboard"
          className="text-text-tertiary hover:text-text-secondary p-1.5 rounded hover:bg-surface-3 transition-colors"
        >
          <RefreshCw size={14} />
        </button>
        <button
          onClick={handleBatch}
          disabled={batchRunning}
          className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-accent text-white text-xs font-medium hover:bg-accent-light transition-colors disabled:opacity-50"
        >
          <FolderSearch size={13} />
          {batchRunning ? 'Analyzing…' : 'Analyze Folder…'}
        </button>
      </div>

      {isEmpty && !batch ? (
        <div className="flex flex-col items-center justify-center gap-2 py-16 rounded-xl border border-border-subtle bg-surface-0">
          <Activity size={22} className="text-text-tertiary" />
          <span className="text-sm text-text-secondary">No analyses yet this session</span>
          <span className="text-2xs text-text-tertiary">Run an analysis or batch-analyze a folder to populate the dashboard</span>
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
              <span key={sev} className={clsx('text-2xs font-bold px-2 py-0.5 rounded-full', sevChip[sev] ?? 'text-text-tertiary bg-surface-3')}>
                {n} {sev}
              </span>
            ))}
            {Object.entries(stats.findingsByCategory).map(([cat, n]) => (
              <span key={cat} className={clsx('text-2xs font-bold px-2 py-0.5 rounded-full', catChip[cat] ?? 'text-text-tertiary bg-surface-3')}>
                {n} {cat}
              </span>
            ))}
          </div>

          {ruleEntries.length > 0 && (
            <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
              <h3 className="text-2xs font-bold uppercase tracking-widest text-text-tertiary mb-2 flex items-center gap-1.5">
                <ShieldAlert size={10} />
                Findings by Rule
              </h3>
              <div className="space-y-1.5">
                {ruleEntries.map(([rule, count]) => (
                  <div key={rule} className="flex items-center gap-2">
                    <span className="text-2xs text-text-secondary font-mono w-44 truncate shrink-0">{rule}</span>
                    <div className="flex-1 h-1.5 bg-surface-3 rounded-full overflow-hidden">
                      <div className="h-full rounded-full bg-brand-500/70" style={{width: `${(count / maxRuleCount) * 100}%`}} />
                    </div>
                    <span className="text-2xs text-text-tertiary tabular-nums w-8 text-right shrink-0">{count}</span>
                  </div>
                ))}
              </div>
            </div>
          )}

          {stats.topProblemFlows?.length > 0 && (
            <div className="p-3 rounded-xl border border-border-subtle bg-surface-0">
              <h3 className="text-2xs font-bold uppercase tracking-widest text-text-tertiary mb-2 flex items-center gap-1.5">
                <FileWarning size={10} />
                Top Problem Flows
              </h3>
              <div className="space-y-1">
                {stats.topProblemFlows.map(p => (
                  <div key={p.flowId} className="flex items-center gap-2 px-2 py-1.5 rounded border border-border-subtle bg-surface-1">
                    <span className="text-2xs text-text-primary flex-1 truncate">{p.flowName || p.flowId.slice(0, 8)}</span>
                    <span className="text-2xs text-text-tertiary tabular-nums">{p.findingCount} findings</span>
                    <span className={clsx('text-2xs font-bold font-mono tabular-nums', scoreColor(p.healthScore))}>{p.healthScore}</span>
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
            <h3 className="text-2xs font-bold uppercase tracking-widest text-text-tertiary flex items-center gap-1.5">
              <FolderSearch size={10} />
              Batch Results
            </h3>
            <div className="flex items-center gap-3">
              <span className="text-2xs text-text-tertiary tabular-nums">
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
            {[...batch.results]
              .sort((a, b) => (b.report?.findings.length ?? -1) - (a.report?.findings.length ?? -1))
              .map((r, i) => (
                <div
                  key={`${r.flowName}-${i}`}
                  className={clsx(
                    'flex items-center gap-2 px-2 py-1.5 rounded border',
                    r.error ? 'border-red-500/20 bg-red-500/5' : 'border-border-subtle bg-surface-1',
                  )}
                >
                  {r.error && <AlertTriangle size={11} className="text-red-400 shrink-0" />}
                  <span className="text-2xs text-text-primary flex-1 truncate">{r.flowName}</span>
                  {r.error ? (
                    <span className="text-2xs text-red-400/90 truncate max-w-[50%]">{r.error}</span>
                  ) : (
                    <>
                      <span className="text-2xs tabular-nums">
                        <span className="text-red-400">{r.report?.stats.errors ?? 0}E</span>{' '}
                        <span className="text-amber-400">{r.report?.stats.warnings ?? 0}W</span>{' '}
                        <span className="text-blue-400">{r.report?.stats.info ?? 0}I</span>
                      </span>
                      <span className={clsx('text-2xs font-bold font-mono tabular-nums w-7 text-right', scoreColor(r.report?.metrics?.healthScore ?? 0))}>
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
