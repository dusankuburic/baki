import {useTranslation} from 'react-i18next'
import {AlertTriangle, RefreshCw} from 'lucide-react'
import {Spinner} from '@/components/shared'
import {ApiKeyMissingState} from '..'

// Announced through the status region below — the row is not a live
// region — so a notice is read once, when it is raised (and only while
// the row is on screen: not over the "add an API key" state).
export function StatusRegion({text}: {text: string}) {
  return (
    <div role="status" className="sr-only">
      {text}
    </div>
  )
}

// Still checking the providers — render nothing rather than flash the
// "add an API key" screen.
export function ProvidersPendingState() {
  return (
    <div className="flex-1 flex items-center justify-center">
      <Spinner />
    </div>
  )
}

// A failed provider check gets a way to retry.
export function ProvidersErrorState({onRetry}: {onRetry: () => void}) {
  const {t} = useTranslation('chat')
  return (
    <div className="flex flex-col items-center justify-center h-full gap-3 p-6 text-center" role="alert">
      <AlertTriangle size={20} className="text-semantic-warning" />
      <div>
        <p className="text-sm font-medium text-text-primary">{t('empty.providersFailedTitle')}</p>
        <p className="text-xs text-text-tertiary mt-1">{t('empty.providersFailedBody')}</p>
      </div>
      <button
        onClick={onRetry}
        className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-surface-3 border border-border-default text-xs text-text-secondary hover:text-text-primary hover:bg-surface-4 transition-colors"
      >
        <RefreshCw size={12} />
        {t('empty.retry')}
      </button>
    </div>
  )
}

// The panel with no providers configured: the "add an API key" state.
export function ProvidersNotConfiguredState() {
  return <ApiKeyMissingState />
}
