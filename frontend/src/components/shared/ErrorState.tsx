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
