import {useTranslation} from 'react-i18next'
import {AlertTriangle, RefreshCw} from 'lucide-react'
import {Spinner} from '@/components/shared'
import {ApiKeyMissingState} from '..'

export function StatusRegion({text}: {text: string}) {
  return (
    <div role="status" className="sr-only">
      {text}
    </div>
  )
}

export function ProvidersPendingState() {
  return (
    <div className="flex-1 flex items-center justify-center">
      <Spinner />
    </div>
  )
}

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

export function ProvidersNotConfiguredState() {
  return <ApiKeyMissingState />
}
