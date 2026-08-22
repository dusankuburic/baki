import {useState, useCallback, useMemo} from 'react'
import {LayoutGrid, AlertCircle, RefreshCw, Download, ChevronDown, Check} from 'lucide-react'
import {libraryApi, type Portfolio, type PortfolioEntry} from '@/api/library'
import {analysisApi} from '@/api'
import {Spinner, EmptyState, useToast} from '@/components/shared'
import {useUIStore} from '@/stores/uiStore'
import {logger} from '@/lib/logger'
import {useAsync} from '@/hooks/useAsync'
import {downloadBlob} from '@/lib/csv'

// healthColor maps a 0–100 health score to a semantic text color.
function healthColor(score: number): string {
  if (score >= 80) return 'text-semantic-success'
  if (score >= 50) return 'text-semantic-warning'
  return 'text-semantic-error'
}

function Stat({label, value, accent}: {label: string; value: string | number; accent?: string}) {
  return (
    <div className="px-4 py-3 rounded-xl border border-border-default bg-surface-1">
      <div className={`text-2xl font-semibold tabular-nums ${accent ?? 'text-text-primary'}`}>{value}</div>
      <div className="text-2xs uppercase tracking-wider text-text-tertiary mt-0.5">{label}</div>
    </div>
  )
}

function exportPortfolioCSV(entries: PortfolioEntry[]): void {
  const header = ['Flow', 'Owner', 'Health', 'Errors', 'Warnings', 'Info', 'Analyzed']
  const rows = entries.map(e => [
    e.flowName || e.flowId,
    e.ownerName || e.ownerId || '',
    e.analyzed ? String(e.healthScore) : '',
    String(e.errors),
    String(e.warnings),
    String(e.info),
    e.analyzedAt ? new Date(e.analyzedAt).toISOString() : '',
  ])
  const csv = [header, ...rows].map(r => r.map(cell => `"${String(cell).replace(/"/g, '""')}"`).join(',')).join('\n')
  downloadBlob(csv, 'text/csv', `portfolio-${new Date().toISOString().slice(0, 10)}.csv`)
}

/**
 * Org-wide governance "fleet view": every flow the user can access, ranked
 * worst-health-first, with rollup totals. Backed by GET /api/library/portfolio
 * (cloud mode). Actionable: select flows for bulk re-analyze or CSV export.
 */
export default function PortfolioView() {
  const setMainPaneView = useUIStore(s => s.setMainPaneView)
  const toast = useToast()
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [reanalyzing, setReanalyzing] = useState<Set<string>>(new Set())
  const {
    data,
    isLoading: loading,
    error,
  } = useAsync<Portfolio>(
    () =>
      libraryApi.portfolio().catch(e => {
        logger.warn('Failed to load portfolio', e)
        throw e
      }),
    [],
  )

  // Memoize so the derived array identity is stable across renders when data
  // is unchanged — the selection/re-analyze callbacks depend on it.
  const entries = useMemo(() => data?.entries ?? [], [data])

  const toggleSelect = useCallback((flowId: string) => {
    setSelected(prev => {
      const next = new Set(prev)
      if (next.has(flowId)) next.delete(flowId)
      else next.add(flowId)
      return next
    })
  }, [])

  const toggleSelectAll = useCallback(() => {
    setSelected(prev => (prev.size === entries.length ? new Set() : new Set(entries.map(e => e.flowId))))
  }, [entries])

  const reanalyzeOne = useCallback(
    async (flowId: string, flowName: string) => {
      setReanalyzing(prev => new Set(prev).add(flowId))
      try {
        await analysisApi.analyzeFlowById(flowId)
        toast.success(`Re-analyzed "${flowName}"`)
      } catch (e) {
        toast.error(`Re-analyze failed for "${flowName}"`, {description: String(e)})
      } finally {
        setReanalyzing(prev => {
          const next = new Set(prev)
          next.delete(flowId)
          return next
        })
      }
    },
    [toast],
  )

  const reanalyzeSelected = useCallback(async () => {
    const ids = [...selected]
    // Sequential to avoid hammering the analyzer / hitting per-user rate limits.
    for (const id of ids) {
      const entry = entries.find(e => e.flowId === id)
      if (entry) await reanalyzeOne(id, entry.flowName || id)
    }
    setSelected(new Set())
  }, [selected, entries, reanalyzeOne])

  const exportSelected = useCallback(() => {
    const subset = entries.filter(e => selected.has(e.flowId))
    exportPortfolioCSV(subset.length > 0 ? subset : entries)
  }, [entries, selected])

  if (loading) {
    return (
      <div className="h-full flex items-center justify-center">
        <Spinner />
      </div>
    )
  }

  if (error) {
    return (
      <div className="h-full flex items-center justify-center p-8">
        <div className="flex items-start gap-3 max-w-md p-4 border border-semantic-error/30 bg-semantic-error/5 rounded-xl">
          <AlertCircle className="text-semantic-error shrink-0 mt-0.5" size={18} />
          <div>
            <p className="text-sm font-medium text-semantic-error">Couldn&apos;t load the portfolio</p>
            <p className="text-xs text-text-tertiary mt-1">{error}</p>
          </div>
        </div>
      </div>
    )
  }

  if (!data || data.entries.length === 0) {
    return (
      <div className="h-full flex items-center justify-center">
        <EmptyState
          icon={LayoutGrid}
          title="No flows to govern yet"
          description="Flows you own or can access will appear here, ranked by health."
        />
      </div>
    )
  }

  const allSelected = selected.size === entries.length && entries.length > 0

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-5xl mx-auto p-6 space-y-6">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-xl font-semibold text-text-primary flex items-center gap-2">
              <LayoutGrid size={20} className="text-brand-500" />
              Flow Portfolio
            </h1>
            <p className="text-sm text-text-secondary mt-1">Every flow you can access, ranked worst-health-first.</p>
          </div>
          <button
            onClick={exportSelected}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg border border-border-default bg-surface-1 text-xs text-text-secondary hover:bg-surface-2 transition-colors shrink-0"
            title="Export the portfolio (or the selected subset) as CSV"
          >
            <Download size={13} />
            Export {selected.size > 0 ? `(${selected.size})` : 'CSV'}
          </button>
        </div>

        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <Stat label="Flows" value={data.totalFlows} />
          <Stat label="Analyzed" value={`${data.analyzedFlows}/${data.totalFlows}`} />
          <Stat
            label="Avg health"
            value={data.analyzedFlows > 0 ? `${data.avgHealth}` : '—'}
            accent={data.analyzedFlows > 0 ? healthColor(data.avgHealth) : undefined}
          />
          <Stat label="Open errors" value={data.errors} accent={data.errors > 0 ? 'text-semantic-error' : undefined} />
        </div>

        {/* Bulk-action toolbar — appears when any rows are selected. */}
        {selected.size > 0 && (
          <div className="flex items-center gap-3 px-4 py-2.5 rounded-xl border border-brand-500/30 bg-brand-500/5">
            <span className="text-sm text-text-secondary">{selected.size} selected</span>
            <button
              onClick={reanalyzeSelected}
              disabled={reanalyzing.size > 0}
              className="flex items-center gap-1.5 px-3 py-1 rounded-lg bg-brand-500 text-brand-foreground text-xs font-medium hover:bg-brand-600 transition-colors disabled:opacity-50"
            >
              <RefreshCw size={12} className={reanalyzing.size > 0 ? 'animate-spin' : ''} />
              Re-analyze {selected.size > 0 ? selected.size : ''}
            </button>
            <button
              onClick={exportSelected}
              className="flex items-center gap-1.5 px-3 py-1 rounded-lg border border-border-default bg-surface-1 text-xs text-text-secondary hover:bg-surface-2 transition-colors"
            >
              <Download size={12} />
              Export selection
            </button>
            <button
              onClick={() => setSelected(new Set())}
              className="ml-auto text-xs text-text-tertiary hover:text-text-secondary"
            >
              Clear
            </button>
          </div>
        )}

        <div className="border border-border-default rounded-xl overflow-x-auto bg-surface-1">
          <table className="w-full text-sm min-w-[680px]">
            <thead>
              <tr className="text-2xs uppercase tracking-wider text-text-tertiary border-b border-border-default">
                <th className="text-left font-medium px-3 py-2 w-8">
                  <button
                    onClick={toggleSelectAll}
                    className="flex items-center justify-center w-4 h-4 rounded border border-border-strong hover:border-brand-500 transition-colors"
                    aria-label={allSelected ? 'Deselect all' : 'Select all'}
                  >
                    {allSelected && <Check size={11} className="text-brand-500" />}
                  </button>
                </th>
                <th className="text-left font-medium px-4 py-2 w-8">#</th>
                <th className="text-left font-medium px-4 py-2">Flow</th>
                <th className="text-left font-medium px-4 py-2">Owner</th>
                <th className="text-right font-medium px-4 py-2">Health</th>
                <th className="text-right font-medium px-4 py-2">Findings</th>
                <th className="text-right font-medium px-4 py-2">Analyzed</th>
                <th className="text-right font-medium px-3 py-2 w-20">Actions</th>
              </tr>
            </thead>
            <tbody>
              {data.entries.map((e, i) => {
                const isSelected = selected.has(e.flowId)
                const isReanalyzing = reanalyzing.has(e.flowId)
                return (
                  <tr
                    key={e.flowId}
                    className={`border-b border-border-subtle last:border-0 hover:bg-surface-2/50 transition-colors ${
                      isSelected ? 'bg-brand-500/5' : ''
                    }`}
                  >
                    <td className="px-3 py-2">
                      <button
                        onClick={() => toggleSelect(e.flowId)}
                        className="flex items-center justify-center w-4 h-4 rounded border border-border-strong hover:border-brand-500 transition-colors"
                        aria-label={`Select ${e.flowName || e.flowId}`}
                      >
                        {isSelected && <Check size={11} className="text-brand-500" />}
                      </button>
                    </td>
                    <td
                      className="px-4 py-2 text-text-tertiary tabular-nums cursor-pointer"
                      onClick={() => setMainPaneView('library')}
                    >
                      {i + 1}
                    </td>
                    <td
                      className="px-4 py-2 text-text-primary truncate max-w-[16rem] cursor-pointer"
                      onClick={() => setMainPaneView('library')}
                      title={e.flowName || e.flowId}
                    >
                      {e.flowName || e.flowId}
                    </td>
                    <td className="px-4 py-2 text-text-tertiary truncate max-w-[12rem]">
                      {e.ownerName || e.ownerId || '—'}
                    </td>
                    <td
                      className={`px-4 py-2 text-right font-semibold tabular-nums ${
                        e.analyzed ? healthColor(e.healthScore) : 'text-text-tertiary'
                      }`}
                    >
                      {e.analyzed ? e.healthScore : '—'}
                    </td>
                    <td className="px-4 py-2 text-right tabular-nums">
                      {e.analyzed ? (
                        <span className="inline-flex gap-2">
                          <span className={e.errors > 0 ? 'text-semantic-error' : 'text-text-tertiary'}>
                            {e.errors}e
                          </span>
                          <span className={e.warnings > 0 ? 'text-semantic-warning' : 'text-text-tertiary'}>
                            {e.warnings}w
                          </span>
                          <span className="text-text-tertiary">{e.info}i</span>
                        </span>
                      ) : (
                        <span className="text-text-tertiary">not analyzed</span>
                      )}
                    </td>
                    <td className="px-4 py-2 text-right text-text-tertiary tabular-nums">
                      {e.analyzedAt ? new Date(e.analyzedAt).toLocaleDateString() : '—'}
                    </td>
                    <td className="px-3 py-2 text-right">
                      <button
                        onClick={() => reanalyzeOne(e.flowId, e.flowName || e.flowId)}
                        disabled={isReanalyzing}
                        className="inline-flex items-center gap-1 px-2 py-1 rounded text-2xs text-text-tertiary hover:text-text-primary hover:bg-surface-3 transition-colors disabled:opacity-50"
                        title="Re-analyze this flow now"
                      >
                        {isReanalyzing ? <RefreshCw size={11} className="animate-spin" /> : <RefreshCw size={11} />}
                        <ChevronDown size={9} className="opacity-40" />
                      </button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  )
}
