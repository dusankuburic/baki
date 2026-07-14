import {LayoutGrid, AlertCircle} from 'lucide-react'
import {libraryApi, type Portfolio} from '@/api/library'
import {Spinner, EmptyState} from '@/components/shared'
import {logger} from '@/lib/logger'
import {useAsync} from '@/hooks/useAsync'

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

/**
 * Org-wide governance "fleet view": every flow the user can access, ranked
 * worst-health-first, with rollup totals. Backed by GET /api/library/portfolio
 * (cloud mode). Read-only for now.
 */
export default function PortfolioView() {
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

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-5xl mx-auto p-6 space-y-6">
        <div>
          <h1 className="text-xl font-semibold text-text-primary flex items-center gap-2">
            <LayoutGrid size={20} className="text-brand-500" />
            Flow Portfolio
          </h1>
          <p className="text-sm text-text-secondary mt-1">Every flow you can access, ranked worst-health-first.</p>
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

        <div className="border border-border-default rounded-xl overflow-hidden bg-surface-1">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-2xs uppercase tracking-wider text-text-tertiary border-b border-border-default">
                <th className="text-left font-medium px-4 py-2 w-8">#</th>
                <th className="text-left font-medium px-4 py-2">Flow</th>
                <th className="text-left font-medium px-4 py-2">Owner</th>
                <th className="text-right font-medium px-4 py-2">Health</th>
                <th className="text-right font-medium px-4 py-2">Findings</th>
                <th className="text-right font-medium px-4 py-2">Analyzed</th>
              </tr>
            </thead>
            <tbody>
              {data.entries.map((e, i) => (
                <tr key={e.flowId} className="border-b border-border-subtle last:border-0 hover:bg-surface-2/50">
                  <td className="px-4 py-2 text-text-tertiary tabular-nums">{i + 1}</td>
                  <td className="px-4 py-2 text-text-primary truncate max-w-[16rem]">{e.flowName || e.flowId}</td>
                  <td className="px-4 py-2 text-text-tertiary truncate max-w-[12rem]">
                    {e.ownerName || e.ownerId || '—'}
                  </td>
                  <td
                    className={`px-4 py-2 text-right font-semibold tabular-nums ${e.analyzed ? healthColor(e.healthScore) : 'text-text-tertiary'}`}
                  >
                    {e.analyzed ? e.healthScore : '—'}
                  </td>
                  <td className="px-4 py-2 text-right tabular-nums">
                    {e.analyzed ? (
                      <span className="inline-flex gap-2">
                        <span className={e.errors > 0 ? 'text-semantic-error' : 'text-text-tertiary'}>{e.errors}e</span>
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
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  )
}
