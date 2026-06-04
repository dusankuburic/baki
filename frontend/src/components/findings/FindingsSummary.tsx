import type {AnalysisStats} from '@/types/domain'
import SeverityBadge from './SeverityBadge'
import clsx from 'clsx'

interface Props {
  stats: AnalysisStats
  durationMs: number
  healthScore?: number
}

function scoreColor(score: number): string {
  if (score >= 80) return 'text-green-400'
  if (score >= 60) return 'text-amber-400'
  if (score >= 40) return 'text-orange-400'
  return 'text-red-400'
}

function scoreBg(score: number): string {
  if (score >= 80) return 'bg-green-500/10'
  if (score >= 60) return 'bg-amber-500/10'
  if (score >= 40) return 'bg-orange-500/10'
  return 'bg-red-500/10'
}

export default function FindingsSummary({stats, durationMs, healthScore}: Props) {
  const total = stats.errors + stats.warnings + stats.info

  return (
    <div className="px-4 py-2.5 border-b border-border-subtle">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          {healthScore !== undefined && (
            <div className={clsx('px-2 py-0.5 rounded-md font-mono text-sm font-bold', scoreBg(healthScore), scoreColor(healthScore))}>
              {healthScore}
            </div>
          )}
          <span className="text-sm font-medium text-text-primary">
            {total} finding{total !== 1 ? 's' : ''}
          </span>
          <div className="flex items-center gap-1.5">
            {stats.errors > 0 && (
              <span className="flex items-center gap-1">
                <SeverityBadge severity="error" />
                <span className="text-2xs text-text-secondary">{stats.errors}</span>
              </span>
            )}
            {stats.warnings > 0 && (
              <span className="flex items-center gap-1">
                <SeverityBadge severity="warning" />
                <span className="text-2xs text-text-secondary">{stats.warnings}</span>
              </span>
            )}
            {stats.info > 0 && (
              <span className="flex items-center gap-1">
                <SeverityBadge severity="info" />
                <span className="text-2xs text-text-secondary">{stats.info}</span>
              </span>
            )}
          </div>
        </div>
        <span className="text-2xs text-text-tertiary">
          {stats.blocksAnalyzed} blocks · {stats.rulesRun} rules · {durationMs}ms
        </span>
      </div>
    </div>
  )
}
