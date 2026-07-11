import clsx from 'clsx'
import type {AnalysisReport, Severity} from '@/types'
import {useAsync} from '@/hooks/useAsync'

// SharedReportView is an UNAUTHENTICATED standalone page that renders a
// read-only findings report via a share token. It bypasses the normal app
// shell (no sidebar, no auth, no stores) so a recipient can review findings
// without an account. The token authorizes access; the backend returns the
// current analysis report for the flow.
interface SharedData {
  flowId: string
  flowName: string
  report: AnalysisReport
}

const sevColor: Record<Severity, string> = {
  error: 'text-red-400 border-red-500/30 bg-red-500/5',
  warning: 'text-amber-400 border-amber-500/30 bg-amber-500/5',
  info: 'text-blue-400 border-blue-500/30 bg-blue-500/5',
}

const sevLabel: Record<Severity, string> = {
  error: 'Error',
  warning: 'Warning',
  info: 'Info',
}

export default function SharedReportView() {
  const {data, isLoading: loading, error} = useAsync<SharedData>(
    () => {
      const params = new URLSearchParams(window.location.search)
      const token = params.get('token')
      if (!token) return Promise.reject(new Error('No share token provided.'))
      return fetch(`/api/shared?token=${encodeURIComponent(token)}`).then(async res => {
        if (!res.ok) throw new Error(res.status === 404 ? 'Invalid or expired link.' : 'Failed to load report.')
        return res.json() as Promise<SharedData>
      })
    },
    [],
  )

  if (loading) {
    return (
      <div className="min-h-screen bg-surface-1 text-text-primary flex items-center justify-center">
        <p className="text-sm text-text-tertiary">Loading report…</p>
      </div>
    )
  }

  if (error) {
    return (
      <div className="min-h-screen bg-surface-1 text-text-primary flex items-center justify-center">
        <div className="text-center space-y-2">
          <p className="text-lg font-medium text-text-primary">Cannot open report</p>
          <p className="text-sm text-text-tertiary">{error}</p>
          <p className="text-xs text-text-disabled mt-4">
            The link may have expired or been revoked. Contact the flow owner for a new link.
          </p>
        </div>
      </div>
    )
  }

  if (!data) return null

  const {report} = data
  const findings = report.findings || []
  const groups = new Map<string, typeof findings>()
  for (const f of findings) {
    const arr = groups.get(f.ruleId)
    if (arr) arr.push(f)
    else groups.set(f.ruleId, [f])
  }
  const sortedGroups = [...groups.entries()].sort((a, b) => {
    const order: Record<string, number> = {error: 0, warning: 1, info: 2}
    return (order[a[1][0].severity] ?? 2) - (order[b[1][0].severity] ?? 2)
  })

  return (
    <div className="min-h-screen bg-surface-1 text-text-primary">
      {/* Header */}
      <div className="border-b border-border-subtle px-6 py-4">
        <div className="max-w-4xl mx-auto">
          <div className="flex items-center gap-2 mb-1">
            <span className="text-lg font-semibold">{data.flowName || 'Shared Report'}</span>
            <span className="text-2xs text-text-disabled uppercase tracking-wider">Read-only</span>
          </div>
          {report.stats && (
            <div className="flex items-center gap-3 text-sm">
              {report.stats.errors > 0 && (
                <span className="text-red-400">{report.stats.errors} error{report.stats.errors !== 1 ? 's' : ''}</span>
              )}
              {report.stats.warnings > 0 && (
                <span className="text-amber-400">{report.stats.warnings} warning{report.stats.warnings !== 1 ? 's' : ''}</span>
              )}
              {report.stats.info > 0 && (
                <span className="text-blue-400">{report.stats.info} info</span>
              )}
              {findings.length === 0 && (
                <span className="text-emerald-400">No findings — the flow looks good!</span>
              )}
              {report.metrics?.healthScore !== undefined && (
                <span className="text-text-tertiary ml-auto">
                  Health: <span className={clsx(
                    'font-medium',
                    report.metrics.healthScore >= 80 ? 'text-emerald-400' :
                    report.metrics.healthScore >= 50 ? 'text-amber-400' : 'text-red-400',
                  )}>{report.metrics.healthScore}/100</span>
                </span>
              )}
            </div>
          )}
        </div>
      </div>

      {/* Findings */}
      <div className="max-w-4xl mx-auto px-6 py-4 space-y-3">
        {sortedGroups.length === 0 ? (
          <div className="text-center py-12 text-text-tertiary">
            No findings detected.
          </div>
        ) : (
          sortedGroups.map(([ruleId, groupFindings]) => (
            <div key={ruleId} className="rounded-lg border border-border-subtle overflow-hidden">
              <div className={clsx('px-4 py-3 border-l-4', sevColor[groupFindings[0].severity])}>
                <div className="flex items-center gap-2 mb-1">
                  <span className="text-sm font-medium">{groupFindings[0].title}</span>
                  <span className="text-2xs text-text-tertiary tabular-nums">{groupFindings.length}×</span>
                  <span className={clsx('text-2xs uppercase tracking-wider px-1.5 py-0.5 rounded border', sevColor[groupFindings[0].severity])}>
                    {sevLabel[groupFindings[0].severity]}
                  </span>
                </div>
                <p className="text-2xs text-text-secondary">{groupFindings[0].description}</p>
                {groupFindings[0].suggestion && (
                  <p className="text-2xs text-emerald-300/70 mt-1">💡 {groupFindings[0].suggestion}</p>
                )}
              </div>
              <div className="divide-y divide-border-subtle">
                {groupFindings.map(f => (
                  <div key={f.id} className="px-4 py-2 flex items-center gap-3">
                    <span className="text-xs font-mono text-text-tertiary">{f.blockId.slice(0, 8)}</span>
                    {f.category && (
                      <span className="text-2xs uppercase tracking-wider text-text-disabled">{f.category}</span>
                    )}
                    {f.confidence && f.confidence !== 'medium' && (
                      <span className={clsx(
                        'text-2xs uppercase px-1 py-0.5 rounded',
                        f.confidence === 'low' ? 'text-amber-400' : 'text-emerald-400',
                      )}>{f.confidence}</span>
                    )}
                  </div>
                ))}
              </div>
            </div>
          ))
        )}
      </div>

      {/* Footer */}
      <div className="border-t border-border-subtle px-6 py-3 mt-8">
        <div className="max-w-4xl mx-auto text-center text-2xs text-text-disabled">
          Report generated {report.generatedAt ? new Date(report.generatedAt).toLocaleString() : ''} ·
          Baki PAD Flow Analyzer
        </div>
      </div>
    </div>
  )
}
