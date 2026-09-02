import {Spinner} from '@/components/shared'
import {useTranslation} from 'react-i18next'

// AnalysisRunner renders the two pre-report UI states of the findings panel:
// the "Run Analysis" call-to-action (no report yet) and the progress spinner
// (analysis in flight). Extracted from FindingsTab so the run/progress
// presentation is isolated from the findings/results view.
//
// When isAnalyzing is true the spinner is shown regardless of whether a stale
// report exists; otherwise the run button is shown (caller guarantees no report
// in that branch).
export interface AnalysisRunnerProps {
  onAnalyze: () => void
  isAnalyzing: boolean
  progress: {current: number; total: number; ruleName: string}
}

export default function AnalysisRunner({onAnalyze, isAnalyzing, progress}: AnalysisRunnerProps) {
  const {t} = useTranslation('findings')
  if (isAnalyzing) {
    // total === 0 → no progress events yet: INDETERMINATE, never "0%".
    const pct = progress.total > 0 ? Math.round((progress.current / progress.total) * 100) : null
    return (
      <div
        className="flex flex-col items-center justify-center h-full gap-3 p-4"
        role="progressbar"
        aria-valuenow={pct ?? undefined}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={t('progress.analysisLabel')}
      >
        <Spinner size={24} />
        <span className="text-sm text-text-secondary tabular-nums">
          {pct === null ? t('progress.analyzingIndeterminate') : t('progress.analyzing', {percent: pct})} ({progress.ruleName})
        </span>
      </div>
    )
  }

  return (
    <div className="flex flex-col items-center justify-center h-full gap-3 p-4">
      <span className="text-sm text-text-tertiary">{t('progress.noRunYet')}</span>
      <button
        onClick={onAnalyze}
        className="px-4 py-2 rounded-lg bg-brand-500 text-brand-foreground text-sm font-medium hover:bg-brand-600 transition-colors"
      >
        {t('progress.runAnalysis')}
      </button>
    </div>
  )
}
