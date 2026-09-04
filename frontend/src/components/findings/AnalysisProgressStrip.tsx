import {useTranslation} from 'react-i18next'
import {Spinner} from '@/components/shared'

// AnalysisProgressStrip is the NON-BLOCKING re-analysis indicator (U1.1): the
// stale findings list stays mounted (scroll position, filters, and expanded
// groups survive) and this slim strip runs above it while a new analysis is
// in flight. Contrast with AnalysisRunner, which owns the PRE-report states
// (no report yet → CTA; initial analysis → full-pane spinner).
//
// total === 0 means no progress events have arrived yet (or the run doesn't
// emit them) — that's INDETERMINATE, not "0%".
export function analysisPercent(progress: {current: number; total: number}): number | null {
  return progress.total > 0 ? Math.min(100, Math.round((progress.current / progress.total) * 100)) : null
}

export default function AnalysisProgressStrip({
  progress,
}: {
  progress: {current: number; total: number; ruleName: string}
}) {
  const {t} = useTranslation('findings')
  const pct = analysisPercent(progress)
  return (
    <div
      className="flex items-center gap-2 px-3 py-1.5 border-b border-border-subtle bg-brand-500/5"
      role="status"
      aria-label={pct === null ? t('progress.inProgressAria') : t('progress.inProgressPercentAria', {percent: pct})}
    >
      <Spinner size={12} />
      <span className="text-2xs text-text-secondary tabular-nums whitespace-nowrap">
        {pct === null ? t('progress.analyzingIndeterminate') : t('progress.analyzing', {percent: pct})}
        {progress.ruleName ? <span className="text-text-tertiary"> · {progress.ruleName}</span> : null}
      </span>
      <div className="flex-1 h-1 min-w-8 bg-surface-3 rounded-full overflow-hidden">
        {pct === null ? (
          <div className="h-full w-1/4 bg-brand-500/70 rounded-full animate-indeterminate" />
        ) : (
          <div className="h-full bg-brand-500 rounded-full transition-all duration-300" style={{width: `${pct}%`}} />
        )}
      </div>
    </div>
  )
}
