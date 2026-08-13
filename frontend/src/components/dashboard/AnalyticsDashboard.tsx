import {useMemo} from 'react'
import {BarChart3, RefreshCw, FolderSearch, AlertTriangle, Activity, ArrowLeft, Workflow} from 'lucide-react'
import {useSystemStore} from '@/stores/systemStore'
import {useUIStore} from '@/stores/uiStore'
import {useFlowStore} from '@/stores/flowStore'
import {formatCount} from '@/lib/format'
import {scoreColor} from '@/lib/scoring'
import {StatCard} from './StatCard'
import {SeverityChips} from './SeverityChips'
import {RuleBarChart} from './RuleBarChart'
import {TopProblemFlows} from './TopProblemFlows'
import {BatchResultsTable} from './BatchResultsTable'
import {useDashboardStats} from './hooks/useDashboardStats'
import {useBatchAnalysis} from './hooks/useBatchAnalysis'

// AnalyticsDashboard is now a thin shell: it gates on the session-analytics
// capability, wires the stats + batch hooks, and renders the presentational
// tiles. All fetch/state-machine logic lives in the hooks; all rendering lives
// in the tile components (StatCard / SeverityChips / RuleBarChart /
// TopProblemFlows / BatchResultsTable).
export default function AnalyticsDashboard() {
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const sessionAnalyticsEnabled = useSystemStore(s => s.info?.capabilities.sessionAnalytics ?? false)
  const isLoaded = useSystemStore(s => s.isLoaded)

  // Session analytics accumulate across single-file loads (only opening a base
  // folder clears the server-side cache); re-fetch when the open document
  // changes so the view reflects the latest state either way.
  const docId = useFlowStore(s => s.document?.id ?? null)
  const {stats, loading, error, refresh} = useDashboardStats(sessionAnalyticsEnabled, isLoaded, docId)
  // Refresh aggregates after a successful batch so the new runs are reflected.
  const {batch, batchRunning, sortedResults, runBatch, exportCSV} = useBatchAnalysis(() => refresh(true))

  const ruleEntries = useMemo(
    () =>
      stats
        ? Object.entries(stats.findingsByRule)
            .sort((a, b) => b[1] - a[1])
            .slice(0, 10)
        : [],
    [stats],
  )
  const maxRuleCount = ruleEntries[0]?.[1] ?? 1
  const isEmpty = !stats || stats.totalFlowsAnalyzed === 0

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
          onClick={() => setMainPaneView('deps')}
          title="Rule dependency graph"
          className="flex items-center gap-1.5 px-2.5 py-1.5 rounded-lg border border-border-subtle text-text-secondary hover:text-text-primary hover:bg-surface-3 text-sm transition-colors"
        >
          <Workflow size={13} />
          Rule Graph
        </button>
        <button
          onClick={runBatch}
          disabled={batchRunning}
          className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-brand-500 text-brand-foreground text-sm font-medium hover:bg-brand-600 transition-colors disabled:opacity-50"
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
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-2">
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
          <span className="text-sm text-text-tertiary">
            Run an analysis or batch-analyze a folder to populate the dashboard
          </span>
          <button
            onClick={runBatch}
            disabled={batchRunning}
            className="mt-3 inline-flex items-center gap-1.5 px-4 py-2 rounded-lg bg-brand-600 hover:bg-brand-700 disabled:opacity-50 text-brand-foreground text-sm font-medium transition-colors"
          >
            {batchRunning ? <RefreshCw size={14} className="animate-spin" /> : <FolderSearch size={14} />}
            {batchRunning ? 'Analyzing…' : 'Analyze folder…'}
          </button>
        </div>
      ) : (
        stats &&
        !isEmpty && (
          <>
            <div className="grid grid-cols-1 sm:grid-cols-3 gap-2">
              <StatCard label="Flows Analyzed" value={formatCount(stats.totalFlowsAnalyzed)} />
              <StatCard label="Total Findings" value={formatCount(stats.totalFindings)} />
              <StatCard label="Avg Health" value={stats.avgHealthScore.toFixed(0)} accent={scoreColor(stats.avgHealthScore)} />
            </div>

            <SeverityChips bySeverity={stats.findingsBySeverity} byCategory={stats.findingsByCategory} />

            <RuleBarChart entries={ruleEntries} maxCount={maxRuleCount} />

            <TopProblemFlows flows={stats.topProblemFlows ?? []} />
          </>
        )
      )}

      {batch && <BatchResultsTable batch={batch} sortedResults={sortedResults} onExport={exportCSV} />}
    </div>
  )
}
