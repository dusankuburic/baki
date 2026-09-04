import {useTranslation} from 'react-i18next'
import {AlertTriangle, RefreshCw} from 'lucide-react'
import EmptyState from './EmptyState'

type ErrorStateProps = {
  title?: string
  message: string
  onRetry?: () => void
  retryLabel?: string
  className?: string
}

/**
 * Error state with an optional Retry action. A thin wrapper over EmptyState so
 * load failures get a consistent, recoverable presentation instead of bare red
 * text. Pass `onRetry` (e.g. the panel's loader callback) to surface a button.
 */
export default function ErrorState({title, message, onRetry, retryLabel, className}: ErrorStateProps) {
  const {t} = useTranslation()
  return (
    <EmptyState
      icon={AlertTriangle}
      title={title ?? t('error')}
      description={message}
      className={className}
      action={
        onRetry && (
          <button
            onClick={onRetry}
            className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-surface-2 hover:bg-surface-3 text-sm text-text-secondary border border-border-default transition-colors"
          >
            <RefreshCw size={13} />
            {retryLabel ?? t('retry')}
          </button>
        )
      }
    />
  )
}
