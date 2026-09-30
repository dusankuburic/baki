import {useTranslation} from 'react-i18next'
import {CheckCircle2, XCircle} from 'lucide-react'
import clsx from 'clsx'
import {formatCount} from '@/lib/format'

/** A draft test's progress and outcome, as a modal's footer shows it. */
export type TestState =
  {phase: 'idle'} | {phase: 'running'} | {phase: 'passed'; latencyMs?: number} | {phase: 'failed'; error?: string}

export const IDLE_TEST: TestState = {phase: 'idle'}

// The outcome under a Test button. Both live regions stay mounted: an outcome
// is inserted into a region that already exists, which is what gets it read
// out. A pass goes to the polite status region; a failure — "Test failed"
// and the server's reason together — is one alert, and the status region is
// emptied so the pass that may have preceded it is not re-read.
export default function TestStatus({state, className}: {state: TestState; className?: string}) {
  const {t} = useTranslation('settings')
  const latency =
    state.phase === 'passed' && typeof state.latencyMs === 'number' && Number.isFinite(state.latencyMs)
      ? state.latencyMs
      : null
  return (
    <div className={clsx('min-w-0 text-xs', className)}>
      <p role="status" className="flex items-center gap-1 text-text-secondary">
        {state.phase === 'running' && t('azureModels.test.running')}
        {state.phase === 'passed' && (
          <>
            <CheckCircle2 size={12} aria-hidden="true" className="shrink-0 text-semantic-success" />
            {latency !== null
              ? t('azureModels.test.passed', {ms: formatCount(latency)})
              : t('azureModels.test.passedNoLatency')}
          </>
        )}
      </p>
      <p role="alert" className="flex items-start gap-1 break-words text-text-primary">
        {state.phase === 'failed' && (
          <>
            <XCircle size={12} aria-hidden="true" className="mt-0.5 shrink-0 text-semantic-error" />
            <span>
              <span className="font-medium">{t('azureModels.test.failed')}</span>{' '}
              <span>{state.error || t('azureModels.test.failedGeneric')}</span>
            </span>
          </>
        )}
      </p>
    </div>
  )
}
